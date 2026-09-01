package app

import (
	"fmt"
	"sort"
	"strings"

	"rcd_manager/internal/calculation"
	"rcd_manager/internal/domain"
)

type indexedPoint struct {
	GroupID   string
	Node      domain.CatalogNode
	SourceIDs []string
}

func filterLowerCalculations(catalogs map[string]domain.SourceCatalog) map[string]domain.SourceCatalog {
	filtered := make(map[string]domain.SourceCatalog, len(catalogs))
	for sourceID, catalog := range catalogs {
		catalog = catalog.Clone()
		nodes := make([]domain.CatalogNode, 0, len(catalog.Nodes))
		for _, node := range catalog.Nodes {
			if node.Kind != "calculation" {
				nodes = append(nodes, node)
			}
		}
		catalog.Nodes = nodes
		filtered[sourceID] = catalog
	}
	return filtered
}

func buildCatalogIndexWithCalculations(catalogs map[string]domain.SourceCatalog, plan *calculation.Plan, retired map[string]indexedPoint, groups []domain.Group) (*catalogIndex, map[string]domain.CatalogState, int, error) {
	input := catalogs
	if plan != nil {
		input = filterLowerCalculations(catalogs)
	}

	// An active central output owns its stable ns=2 NodeId. If a lower later
	// publishes a raw/command node with that identifier, isolate that source
	// instead of making the manager (and the last valid calculation version)
	// unavailable. Lower calculation nodes are intentionally ignored above.
	calculationConflicts := make(map[string][]string)
	if plan != nil {
		outputs := make(map[string]struct{}, plan.RuleCount()+len(retired))
		for _, node := range plan.CatalogNodes() {
			outputs[node.Key.Identifier] = struct{}{}
		}
		for identifier := range retired {
			outputs[identifier] = struct{}{}
		}
		accepted := make(map[string]domain.SourceCatalog, len(input))
		for sourceID, catalog := range input {
			for _, node := range catalog.Nodes {
				if _, exists := outputs[node.Key.Identifier]; exists {
					calculationConflicts[sourceID] = append(calculationConflicts[sourceID], node.Key.Identifier)
				}
			}
			if len(calculationConflicts[sourceID]) == 0 {
				accepted[sourceID] = catalog
			}
		}
		input = accepted
	}

	index, states, conflicts, err := validateCatalogs(input)
	if err != nil || plan == nil {
		return index, states, conflicts, err
	}
	for sourceID, identifiers := range calculationConflicts {
		sort.Strings(identifiers)
		states[sourceID] = domain.CatalogConflict
		index.union.SourceStates[sourceID] = domain.CatalogConflict
		index.union.Conflicts = append(index.union.Conflicts, domain.CatalogConflictDetail{
			Kind:       domain.ConflictDuplicateNode,
			Identifier: strings.Join(identifiers, ","),
			SourceIDs:  []string{sourceID},
			Message:    "lower node conflicts with an active manager calculation output",
		})
		conflicts++
	}
	if err := index.addManagerCalculations(plan, retired, groups); err != nil {
		return nil, nil, 0, err
	}
	return index, states, conflicts, nil
}

func (c *catalogIndex) addManagerCalculations(plan *calculation.Plan, retired map[string]indexedPoint, groups []domain.Group) error {
	if c == nil || plan == nil || plan.RuleCount() == 0 {
		if c == nil || len(retired) == 0 {
			return nil
		}
	}
	groupIDs := make([]string, 0, len(groups))
	for _, group := range groups {
		groupIDs = append(groupIDs, group.ID)
	}
	sort.Strings(groupIDs)
	if len(groupIDs) == 0 {
		return fmt.Errorf("calculation configuration requires at least one logical group")
	}
	fallback := groupIDs[0]
	for _, groupID := range groupIDs {
		if groupID == "shared" || groupID == "calculations" {
			fallback = groupID
			break
		}
	}
	resolvedGroups := make(map[string]string, plan.RuleCount())
	nodes := plan.CatalogNodes()
	rules := plan.RuleViews()
	for index, node := range nodes {
		owners := make(map[string]struct{})
		for _, dependency := range rules[index].Dependencies {
			if groupID := resolvedGroups[dependency]; groupID != "" {
				owners[groupID] = struct{}{}
			} else if groupID, exists := c.groupByNode[dependency]; exists {
				owners[groupID] = struct{}{}
			}
		}
		groupID := fallback
		if len(owners) == 1 {
			for owner := range owners {
				groupID = owner
			}
		}
		resolvedGroups[node.Key.Identifier] = groupID
		if err := c.addManagerCalculationPoint(indexedPoint{GroupID: groupID, Node: node}, fallback, groupIDs); err != nil {
			return err
		}
	}
	retiredIDs := make([]string, 0, len(retired))
	for identifier := range retired {
		retiredIDs = append(retiredIDs, identifier)
	}
	sort.Strings(retiredIDs)
	for _, identifier := range retiredIDs {
		if _, active := resolvedGroups[identifier]; active {
			continue
		}
		point := retired[identifier]
		point.Node.Retired = true
		if err := c.addManagerCalculationPoint(point, fallback, groupIDs); err != nil {
			return err
		}
	}
	sort.Slice(c.points, func(i, j int) bool {
		if c.points[i].GroupID == c.points[j].GroupID {
			return c.points[i].Node.Key.Identifier < c.points[j].Node.Key.Identifier
		}
		return c.points[i].GroupID < c.points[j].GroupID
	})
	return nil
}

func (c *catalogIndex) addManagerCalculationPoint(point indexedPoint, fallback string, groups []string) error {
	identifier := point.Node.Key.Identifier
	if existing, exists := c.pointByNode[identifier]; exists {
		return fmt.Errorf("calculation output %q conflicts with node in group %q", identifier, existing.GroupID)
	}
	if index := sort.SearchStrings(groups, point.GroupID); point.GroupID == "" || index >= len(groups) || groups[index] != point.GroupID {
		point.GroupID = fallback
	}
	point.SourceIDs = nil
	c.points = append(c.points, point)
	c.pointByNode[identifier] = point
	c.groupByNode[identifier] = point.GroupID
	c.groupPoints[point.GroupID] = append(c.groupPoints[point.GroupID], point)
	if c.union.Groups[point.GroupID] == nil {
		c.union.Groups[point.GroupID] = make(map[string]domain.CatalogNode)
	}
	c.union.Groups[point.GroupID][identifier] = point.Node
	return nil
}

func calculationInputs(index *catalogIndex) map[string]calculation.InputDefinition {
	if index == nil {
		return nil
	}
	result := make(map[string]calculation.InputDefinition)
	for _, point := range index.points {
		if point.Node.Kind == "calculation" || point.Node.Kind == "manager_calculation" {
			continue
		}
		definition := calculation.InputDefinition{DataType: point.Node.DataType, ValueRank: point.Node.ValueRank}
		if point.Node.Class != domain.NodeClassVariable || point.Node.Writable {
			definition = calculation.InputDefinition{DataType: domain.DataTypeString, ValueRank: 0}
		}
		result[point.Node.Key.Identifier] = definition
	}
	return result
}

type catalogIndex struct {
	union       domain.CatalogUnion
	points      []indexedPoint
	pointByNode map[string]indexedPoint
	groupByNode map[string]string
	groupPoints map[string][]indexedPoint
	sourceNodes map[string]map[string]domain.CatalogNode
}

func buildCatalogIndex(catalogs []domain.SourceCatalog) (*catalogIndex, error) {
	union, err := domain.BuildCatalogUnion(catalogs)
	if err != nil {
		return nil, err
	}
	index := &catalogIndex{
		union:       union,
		pointByNode: make(map[string]indexedPoint),
		groupByNode: make(map[string]string),
		groupPoints: make(map[string][]indexedPoint),
		sourceNodes: make(map[string]map[string]domain.CatalogNode),
	}
	for _, catalog := range catalogs {
		nodes := make(map[string]domain.CatalogNode, len(catalog.Nodes))
		for _, node := range catalog.Nodes {
			nodes[node.Key.Identifier] = node
		}
		index.sourceNodes[catalog.SourceID] = nodes
	}
	for groupID, nodes := range union.Groups {
		for identifier, node := range nodes {
			point := indexedPoint{GroupID: groupID, Node: node}
			allRetired := true
			for sourceID, sourceNodes := range index.sourceNodes {
				if sourceNode, ok := sourceNodes[identifier]; ok {
					point.SourceIDs = append(point.SourceIDs, sourceID)
					if !sourceNode.Retired {
						allRetired = false
					}
				}
			}
			point.Node.Retired = allRetired
			// BuildCatalogUnion chooses a deterministic owner shape, whose retired
			// bit may belong to only one source. Retirement is a union property: the
			// public node remains active while any group member still provides it.
			unionNode := index.union.Groups[groupID][identifier]
			unionNode.Retired = allRetired
			index.union.Groups[groupID][identifier] = unionNode
			sort.Strings(point.SourceIDs)
			index.points = append(index.points, point)
			index.pointByNode[identifier] = point
			index.groupByNode[identifier] = groupID
			index.groupPoints[groupID] = append(index.groupPoints[groupID], point)
		}
	}
	sort.Slice(index.points, func(i, j int) bool {
		if index.points[i].GroupID == index.points[j].GroupID {
			return index.points[i].Node.Key.Identifier < index.points[j].Node.Key.Identifier
		}
		return index.points[i].GroupID < index.points[j].GroupID
	})
	return index, nil
}

func (c *catalogIndex) sourceNode(sourceID, identifier string) (domain.CatalogNode, bool) {
	if c == nil {
		return domain.CatalogNode{}, false
	}
	node, ok := c.sourceNodes[sourceID][identifier]
	return node, ok
}

func (c *catalogIndex) pointForNode(identifier string) (indexedPoint, bool) {
	if c == nil {
		return indexedPoint{}, false
	}
	point, ok := c.pointByNode[identifier]
	return point, ok
}

func (c *catalogIndex) groupForNode(identifier string) (string, bool) {
	if c == nil {
		return "", false
	}
	group, ok := c.groupByNode[identifier]
	return group, ok
}

func (c *catalogIndex) sourceHasNode(sourceID, identifier string) bool {
	if c == nil {
		return false
	}
	_, ok := c.sourceNodes[sourceID][identifier]
	return ok
}

func (c *catalogIndex) filteredPoints(groupID, sourceID, status, query string) []indexedPoint {
	if c == nil {
		return nil
	}
	query = strings.ToLower(strings.TrimSpace(query))
	result := make([]indexedPoint, 0)
	for _, point := range c.points {
		if groupID != "" && point.GroupID != groupID {
			continue
		}
		if sourceID != "" && !containsString(point.SourceIDs, sourceID) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(point.Node.Key.Identifier+" "+point.Node.BrowseName), query) {
			continue
		}
		// Status filtering is joined against live samples by Runtime.ListPoints.
		_ = status
		result = append(result, point)
	}
	return result
}

func containsString(values []string, target string) bool {
	index := sort.SearchStrings(values, target)
	return index < len(values) && values[index] == target
}
