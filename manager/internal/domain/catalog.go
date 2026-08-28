package domain

import (
	"fmt"
	"sort"
	"strings"
)

type CatalogConflictKind string

const (
	ConflictDuplicateNode   CatalogConflictKind = "DuplicateNode"
	ConflictCrossGroup      CatalogConflictKind = "CrossGroupDuplicate"
	ConflictTypeMismatch    CatalogConflictKind = "TypeMismatch"
	ConflictMethodSignature CatalogConflictKind = "MethodSignature"
	ConflictCommandVariable CatalogConflictKind = "CommandVariable"
)

type CatalogConflictDetail struct {
	Kind       CatalogConflictKind `json:"kind"`
	GroupID    string              `json:"group_id"`
	Identifier string              `json:"identifier"`
	SourceIDs  []string            `json:"source_ids"`
	Message    string              `json:"message"`
}

type CatalogUnion struct {
	Groups       map[string]map[string]CatalogNode `json:"groups"`
	SourceStates map[string]CatalogState           `json:"source_states"`
	Conflicts    []CatalogConflictDetail           `json:"conflicts"`
}

type nodeOwner struct {
	node          CatalogNode
	sourceIDs     []string
	conflictIndex int
}

// BuildCatalogUnion builds the canonical per-group point set. A duplicate node
// identifier across groups is a configuration error. Conflicts inside one group
// are retained for diagnostics and every implicated source becomes unselectable.
func BuildCatalogUnion(catalogs []SourceCatalog) (CatalogUnion, error) {
	result := CatalogUnion{
		Groups:       make(map[string]map[string]CatalogNode),
		SourceStates: make(map[string]CatalogState),
	}
	groupByIdentifier := make(map[string]string)
	owners := make(map[string]map[string]*nodeOwner)
	ordered := append([]SourceCatalog(nil), catalogs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].GroupID == ordered[j].GroupID {
			return ordered[i].SourceID < ordered[j].SourceID
		}
		return ordered[i].GroupID < ordered[j].GroupID
	})

	for _, catalog := range ordered {
		if strings.TrimSpace(catalog.SourceID) == "" {
			return CatalogUnion{}, &FieldError{Field: "source_id", Message: "must not be empty"}
		}
		if strings.TrimSpace(catalog.GroupID) == "" {
			return CatalogUnion{}, &FieldError{Field: "group_id", Message: "must not be empty"}
		}
		if _, duplicate := result.SourceStates[catalog.SourceID]; duplicate {
			return CatalogUnion{}, fmt.Errorf("source %q: %w", catalog.SourceID, ErrAlreadyExists)
		}
		state := catalog.State
		if state == "" {
			state = CatalogDiscovering
		}
		result.SourceStates[catalog.SourceID] = state
		if result.Groups[catalog.GroupID] == nil {
			result.Groups[catalog.GroupID] = make(map[string]CatalogNode)
			owners[catalog.GroupID] = make(map[string]*nodeOwner)
		}

		seen := make(map[string]CatalogNode)
		nodes := append([]CatalogNode(nil), catalog.Nodes...)
		sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Key.Identifier < nodes[j].Key.Identifier })
		for _, node := range nodes {
			identifier := strings.TrimSpace(node.Key.Identifier)
			if identifier == "" {
				markConflict(&result, CatalogConflictDetail{
					Kind: ConflictDuplicateNode, GroupID: catalog.GroupID,
					SourceIDs: []string{catalog.SourceID}, Message: "empty node identifier",
				})
				continue
			}
			if otherGroup, ok := groupByIdentifier[identifier]; ok && otherGroup != catalog.GroupID {
				return CatalogUnion{}, &CrossGroupDuplicateError{
					Identifier: identifier, FirstGroup: otherGroup, OtherGroup: catalog.GroupID,
				}
			}
			groupByIdentifier[identifier] = catalog.GroupID

			if previous, duplicate := seen[identifier]; duplicate {
				kind := ConflictDuplicateNode
				if !sameShape(previous, node) {
					kind = ConflictTypeMismatch
				}
				markConflict(&result, CatalogConflictDetail{
					Kind: kind, GroupID: catalog.GroupID, Identifier: identifier,
					SourceIDs: []string{catalog.SourceID}, Message: "source contains duplicate node identifier",
				})
				continue
			}
			seen[identifier] = node

			if node.Class == NodeClassMethod && !validCommandMethod(node) {
				markConflict(&result, CatalogConflictDetail{
					Kind: ConflictMethodSignature, GroupID: catalog.GroupID, Identifier: identifier,
					SourceIDs: []string{catalog.SourceID}, Message: "method must have HexData String input and Result String output",
				})
				continue
			}
			if node.Class == NodeClassVariable && strings.HasSuffix(identifier, "_Send") && !validCommandVariable(node) {
				markConflict(&result, CatalogConflictDetail{
					Kind: ConflictCommandVariable, GroupID: catalog.GroupID, Identifier: identifier,
					SourceIDs: []string{catalog.SourceID}, Message: "_Send variable must be a writable scalar String",
				})
				continue
			}

			owner := owners[catalog.GroupID][identifier]
			if owner == nil {
				owners[catalog.GroupID][identifier] = &nodeOwner{node: node, sourceIDs: []string{catalog.SourceID}, conflictIndex: -1}
				result.Groups[catalog.GroupID][identifier] = node
				continue
			}
			if owner.conflictIndex >= 0 {
				result.Conflicts[owner.conflictIndex].SourceIDs = append(result.Conflicts[owner.conflictIndex].SourceIDs, catalog.SourceID)
			} else if !sameShape(owner.node, node) {
				ids := append(append([]string{}, owner.sourceIDs...), catalog.SourceID)
				owner.conflictIndex = len(result.Conflicts)
				markConflict(&result, CatalogConflictDetail{
					Kind: ConflictTypeMismatch, GroupID: catalog.GroupID, Identifier: identifier,
					SourceIDs: ids, Message: "same-name nodes have incompatible class, type, rank, or method signature",
				})
			}
			owner.sourceIDs = append(owner.sourceIDs, catalog.SourceID)
		}
	}

	for i := range result.Conflicts {
		sort.Strings(result.Conflicts[i].SourceIDs)
		result.Conflicts[i].SourceIDs = uniqueStrings(result.Conflicts[i].SourceIDs)
		for _, sourceID := range result.Conflicts[i].SourceIDs {
			result.SourceStates[sourceID] = CatalogConflict
		}
	}
	sort.SliceStable(result.Conflicts, func(i, j int) bool {
		left, right := result.Conflicts[i], result.Conflicts[j]
		if left.GroupID != right.GroupID {
			return left.GroupID < right.GroupID
		}
		if left.Identifier != right.Identifier {
			return left.Identifier < right.Identifier
		}
		return left.Kind < right.Kind
	})
	return result, nil
}

func markConflict(result *CatalogUnion, conflict CatalogConflictDetail) {
	result.Conflicts = append(result.Conflicts, conflict)
}

func sameShape(a, b CatalogNode) bool {
	if a.Class != b.Class || a.DataType != b.DataType || a.ValueRank != b.ValueRank {
		return false
	}
	if a.Class != NodeClassMethod {
		return true
	}
	return sameArguments(a.InputArguments, b.InputArguments) && sameArguments(a.OutputArguments, b.OutputArguments)
}

func sameArguments(a, b []Argument) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validCommandMethod(node CatalogNode) bool {
	if !strings.HasSuffix(node.Key.Identifier, "_SendCommand") || len(node.InputArguments) != 1 || len(node.OutputArguments) != 1 {
		return false
	}
	in, out := node.InputArguments[0], node.OutputArguments[0]
	return in.Name == "HexData" && in.DataType == DataTypeString && in.ValueRank == -1 &&
		out.Name == "Result" && out.DataType == DataTypeString && out.ValueRank == -1
}

func validCommandVariable(node CatalogNode) bool {
	return node.DataType == DataTypeString && node.ValueRank == -1 && node.Writable
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
