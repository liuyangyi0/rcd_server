package app

import (
	"sort"
	"strings"

	"rcd_manager/internal/domain"
)

type indexedPoint struct {
	GroupID   string
	Node      domain.CatalogNode
	SourceIDs []string
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
