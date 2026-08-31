package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"rcd_manager/internal/domain"
)

// ReplaceSourceCatalog atomically replaces one source's cached schema. It does
// not bump the source configuration revision because discovery is runtime state.
func (r *Repository) ReplaceSourceCatalog(ctx context.Context, catalog domain.SourceCatalog) error {
	if catalog.SourceID == "" {
		return &domain.FieldError{Field: "source_id", Message: "must not be empty"}
	}
	if catalog.State == "" {
		return &domain.FieldError{Field: "state", Message: "must not be empty"}
	}
	if catalog.QuarantineReason != "" {
		catalog.State = domain.CatalogConflict
	}
	source, err := r.GetSource(ctx, catalog.SourceID)
	if err != nil {
		return err
	}
	if catalog.GroupID != "" && catalog.GroupID != source.GroupID {
		return &domain.FieldError{Field: "group_id", Message: "does not match the persisted source"}
	}
	if catalog.DiscoveredAt.IsZero() {
		catalog.DiscoveredAt = r.clock().UTC()
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin catalog replace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_nodes WHERE source_id = ?`, catalog.SourceID); err != nil {
		return fmt.Errorf("clear source catalog: %w", err)
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO catalog_nodes
		(source_id, namespace_uri, identifier, browse_name, parent_path, node_class, data_type, value_rank,
		 writable, input_arguments, output_arguments, retired, discovered_at, group_id, port_id, device_id, kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare catalog insert: %w", err)
	}
	defer statement.Close()
	for _, node := range catalog.Nodes {
		inputs, err := json.Marshal(node.InputArguments)
		if err != nil {
			return fmt.Errorf("encode input arguments: %w", err)
		}
		outputs, err := json.Marshal(node.OutputArguments)
		if err != nil {
			return fmt.Errorf("encode output arguments: %w", err)
		}
		if _, err := statement.ExecContext(ctx, catalog.SourceID, node.Key.NamespaceURI, node.Key.Identifier,
			node.BrowseName, node.ParentPath, node.Class, node.DataType, node.ValueRank, boolInt(node.Writable),
			string(inputs), string(outputs), boolInt(node.Retired), encodeTime(catalog.DiscoveredAt), node.GroupID, node.PortID, node.DeviceID, node.Kind); err != nil {
			return fmt.Errorf("insert catalog node %q: %w", node.Key.Identifier, err)
		}
	}
	topologyJSON, err := json.Marshal(catalog.Topology)
	if err != nil {
		return fmt.Errorf("encode source topology: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO source_topology (source_id,manifest,discovered_at,quarantine_reason) VALUES (?,?,?,?)
		ON CONFLICT(source_id) DO UPDATE SET manifest=excluded.manifest,discovered_at=excluded.discovered_at,
		quarantine_reason=excluded.quarantine_reason`, catalog.SourceID, string(topologyJSON), encodeTime(catalog.DiscoveredAt), catalog.QuarantineReason); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE sources SET catalog_state = ? WHERE id = ?`, catalog.State, catalog.SourceID)
	if err != nil {
		return fmt.Errorf("update catalog state: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return domain.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit catalog replace: %w", err)
	}
	return nil
}

func (r *Repository) LoadSourceCatalog(ctx context.Context, sourceID string) (domain.SourceCatalog, error) {
	source, err := r.GetSource(ctx, sourceID)
	if err != nil {
		return domain.SourceCatalog{}, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT namespace_uri, identifier, browse_name, parent_path, node_class,
		data_type, value_rank, writable, input_arguments, output_arguments, retired, discovered_at, group_id, port_id, device_id, kind
		FROM catalog_nodes WHERE source_id = ? ORDER BY identifier`, sourceID)
	if err != nil {
		return domain.SourceCatalog{}, fmt.Errorf("load source catalog: %w", err)
	}
	defer rows.Close()
	catalog := domain.SourceCatalog{SourceID: source.ID, GroupID: source.GroupID, State: source.CatalogState}
	for rows.Next() {
		var node domain.CatalogNode
		var writable, retired int
		var inputs, outputs, discovered string
		if err := rows.Scan(&node.Key.NamespaceURI, &node.Key.Identifier, &node.BrowseName, &node.ParentPath,
			&node.Class, &node.DataType, &node.ValueRank, &writable, &inputs, &outputs, &retired, &discovered, &node.GroupID, &node.PortID, &node.DeviceID, &node.Kind); err != nil {
			return domain.SourceCatalog{}, fmt.Errorf("scan catalog node: %w", err)
		}
		node.Writable = writable != 0
		node.Retired = retired != 0
		if err := json.Unmarshal([]byte(inputs), &node.InputArguments); err != nil {
			return domain.SourceCatalog{}, fmt.Errorf("decode input arguments: %w", err)
		}
		if err := json.Unmarshal([]byte(outputs), &node.OutputArguments); err != nil {
			return domain.SourceCatalog{}, fmt.Errorf("decode output arguments: %w", err)
		}
		catalog.Nodes = append(catalog.Nodes, node)
		if catalog.DiscoveredAt.IsZero() {
			catalog.DiscoveredAt, err = decodeTime(discovered)
			if err != nil {
				return domain.SourceCatalog{}, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return domain.SourceCatalog{}, err
	}
	if err := rows.Close(); err != nil {
		return domain.SourceCatalog{}, err
	}
	var manifest, discovered string
	err = r.db.QueryRowContext(ctx, `SELECT manifest,discovered_at,quarantine_reason FROM source_topology WHERE source_id=?`, sourceID).
		Scan(&manifest, &discovered, &catalog.QuarantineReason)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.SourceCatalog{}, err
	}
	if err == nil {
		if err := json.Unmarshal([]byte(manifest), &catalog.Topology); err != nil {
			return domain.SourceCatalog{}, fmt.Errorf("decode cached topology: %w", err)
		}
		if catalog.DiscoveredAt.IsZero() {
			catalog.DiscoveredAt, err = decodeTime(discovered)
			if err != nil {
				return domain.SourceCatalog{}, err
			}
		}
	}
	return catalog, nil
}

type CatalogRecord struct {
	SourceID string
	GroupID  string
	Node     domain.CatalogNode
}

type CatalogFilter struct {
	GroupID  string
	SourceID string
	Query    string
	Limit    int
	Offset   int
}

// ListCatalogRecords supports the management point browser. Runtime values are
// intentionally not fabricated here; the service layer may join real cache data.
func (r *Repository) ListCatalogRecords(ctx context.Context, groupID, query string, limit, offset int) ([]CatalogRecord, int, error) {
	return r.ListCatalogRecordsFiltered(ctx, CatalogFilter{GroupID: groupID, Query: query, Limit: limit, Offset: offset})
}

func (r *Repository) ListCatalogRecordsFiltered(ctx context.Context, filter CatalogFilter) ([]CatalogRecord, int, error) {
	if filter.Limit <= 0 || filter.Limit > 1000 {
		filter.Limit = 100
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	where := ` WHERE (? = '' OR COALESCE(NULLIF(n.group_id,''),s.group_id) = ?) AND (? = '' OR n.source_id = ?)
		AND (? = '' OR n.identifier LIKE '%' || ? || '%' OR n.browse_name LIKE '%' || ? || '%')`
	args := []any{filter.GroupID, filter.GroupID, filter.SourceID, filter.SourceID, filter.Query, filter.Query, filter.Query}
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_nodes n JOIN sources s ON s.id = n.source_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count catalog records: %w", err)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT n.source_id, COALESCE(NULLIF(n.group_id,''),s.group_id), n.namespace_uri, n.identifier, n.browse_name,
		n.parent_path, n.node_class, n.data_type, n.value_rank, n.writable, n.input_arguments,
		n.output_arguments, n.retired, n.group_id,n.port_id,n.device_id,n.kind FROM catalog_nodes n JOIN sources s ON s.id = n.source_id`+where+
		` ORDER BY s.group_id, n.identifier, n.source_id LIMIT ? OFFSET ?`, append(args, filter.Limit, filter.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list catalog records: %w", err)
	}
	defer rows.Close()
	records := make([]CatalogRecord, 0, filter.Limit)
	for rows.Next() {
		var record CatalogRecord
		var writable, retired int
		var inputs, outputs string
		if err := rows.Scan(&record.SourceID, &record.GroupID, &record.Node.Key.NamespaceURI,
			&record.Node.Key.Identifier, &record.Node.BrowseName, &record.Node.ParentPath, &record.Node.Class,
			&record.Node.DataType, &record.Node.ValueRank, &writable, &inputs, &outputs, &retired, &record.Node.GroupID, &record.Node.PortID, &record.Node.DeviceID, &record.Node.Kind); err != nil {
			return nil, 0, err
		}
		record.Node.Writable = writable != 0
		record.Node.Retired = retired != 0
		if err := json.Unmarshal([]byte(inputs), &record.Node.InputArguments); err != nil {
			return nil, 0, err
		}
		if err := json.Unmarshal([]byte(outputs), &record.Node.OutputArguments); err != nil {
			return nil, 0, err
		}
		records = append(records, record)
	}
	return records, total, rows.Err()
}
