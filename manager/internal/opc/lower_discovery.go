package opc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	gopcua "github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/domain"
)

func namespaceIndex(namespaces []string) map[string]uint16 {
	result := make(map[string]uint16, len(namespaces))
	for i, uri := range namespaces {
		result[uri] = uint16(i)
	}
	return result
}

func discoverCatalog(
	ctx context.Context,
	client *gopcua.Client,
	source domain.Source,
	namespaces map[string]uint16,
) (domain.SourceCatalog, map[string]observedNode, error) {
	ns, ok := namespaces[BusinessNamespaceURI]
	if !ok {
		return domain.SourceCatalog{}, nil, fmt.Errorf("namespace %q is missing", BusinessNamespaceURI)
	}
	devicesID := gu.NewStringNodeID(ns, "Devices")
	deviceRefs, err := browseForwardReferences(ctx, client, devicesID, id.HasComponent, gu.NodeClassVariable)
	if err != nil {
		return domain.SourceCatalog{}, nil, fmt.Errorf("browse Devices: %w", err)
	}
	variables, observed, err := discoverVariables(ctx, client, source, namespaces, deviceRefs)
	if err != nil {
		return domain.SourceCatalog{}, nil, err
	}

	methodsID := gu.NewStringNodeID(ns, "Methods")
	methodRefs, err := browseForwardReferences(ctx, client, methodsID, id.HasComponent, gu.NodeClassMethod)
	if err != nil {
		return domain.SourceCatalog{}, nil, fmt.Errorf("browse Methods: %w", err)
	}
	methods := make([]domain.CatalogNode, 0, len(methodRefs))
	for _, ref := range methodRefs {
		method, err := discoverMethod(ctx, client, namespaces, ref)
		if err != nil {
			return domain.SourceCatalog{}, nil, err
		}
		methods = append(methods, method)
	}
	nodes := append(variables, methods...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Key.Identifier < nodes[j].Key.Identifier })
	topology, err := discoverTopology(ctx, client, namespaces, nodes)
	if err != nil {
		return domain.SourceCatalog{}, nil, err
	}
	return domain.SourceCatalog{
		SourceID: source.ID, GroupID: source.GroupID, State: domain.CatalogReady,
		DiscoveredAt: time.Now().UTC(), Nodes: nodes, Topology: topology,
	}, observed, nil
}

func discoverVariables(
	ctx context.Context,
	client *gopcua.Client,
	source domain.Source,
	namespaces map[string]uint16,
	refs []*gu.ReferenceDescription,
) ([]domain.CatalogNode, map[string]observedNode, error) {
	_ = source
	nodes := make([]domain.CatalogNode, len(refs))
	observed := make(map[string]observedNode, len(refs))
	const attrsPerNode = 3
	// Attribute reads return three DataValues per node. Keeping this batch
	// smaller than the monitored-item batch avoids oversized Read responses on
	// the Rust server's real ~5900-node address space.
	const batchNodes = 100
	for start := 0; start < len(refs); start += batchNodes {
		end := start + batchNodes
		if end > len(refs) {
			end = len(refs)
		}
		request := &gu.ReadRequest{NodesToRead: make([]*gu.ReadValueID, 0, (end-start)*attrsPerNode)}
		for _, ref := range refs[start:end] {
			nodeID := ref.NodeID.NodeID
			request.NodesToRead = append(request.NodesToRead,
				&gu.ReadValueID{NodeID: nodeID, AttributeID: gu.AttributeIDDataType},
				&gu.ReadValueID{NodeID: nodeID, AttributeID: gu.AttributeIDValueRank},
				&gu.ReadValueID{NodeID: nodeID, AttributeID: gu.AttributeIDAccessLevel},
			)
		}
		response, err := client.Read(ctx, request)
		if err != nil {
			return nil, nil, fmt.Errorf("read variable attributes: %w", err)
		}
		if response == nil || len(response.Results) != len(request.NodesToRead) {
			return nil, nil, errors.New("read variable attributes returned an incomplete result")
		}
		for local, ref := range refs[start:end] {
			offset := local * attrsPerNode
			dataType, err := dataTypeFromAttribute(response.Results[offset])
			if err != nil {
				return nil, nil, fmt.Errorf("node %s data type: %w", ref.BrowseName.Name, err)
			}
			valueRank, err := int32Attribute(response.Results[offset+1])
			if err != nil {
				return nil, nil, fmt.Errorf("node %s value rank: %w", ref.BrowseName.Name, err)
			}
			access, err := byteAttribute(response.Results[offset+2])
			if err != nil {
				return nil, nil, fmt.Errorf("node %s access level: %w", ref.BrowseName.Name, err)
			}
			key, err := stableKey(namespaces, ref.NodeID.NodeID)
			if err != nil {
				return nil, nil, err
			}
			node := domain.CatalogNode{
				Key: key, BrowseName: ref.BrowseName.Name, ParentPath: "Objects/Devices",
				Class: domain.NodeClassVariable, DataType: dataType, ValueRank: valueRank,
				Writable: gu.AccessLevelType(access)&gu.AccessLevelTypeCurrentWrite != 0,
			}
			nodes[start+local] = node
			observed[ref.NodeID.NodeID.String()] = observedNode{key: key, dataType: dataType, command: isCommandVariable(node)}
		}
	}
	return nodes, observed, nil
}

func isCommandVariable(node domain.CatalogNode) bool {
	return node.Key.NamespaceURI == BusinessNamespaceURI && node.Class == domain.NodeClassVariable &&
		node.Writable && node.DataType == domain.DataTypeString && node.ValueRank == -1 &&
		strings.HasSuffix(node.Key.Identifier, "_Send")
}

func discoverMethod(
	ctx context.Context,
	client *gopcua.Client,
	namespaces map[string]uint16,
	ref *gu.ReferenceDescription,
) (domain.CatalogNode, error) {
	key, err := stableKey(namespaces, ref.NodeID.NodeID)
	if err != nil {
		return domain.CatalogNode{}, err
	}
	node := domain.CatalogNode{
		Key: key, BrowseName: ref.BrowseName.Name, ParentPath: "Objects/Methods",
		Class: domain.NodeClassMethod, ValueRank: -1,
	}
	properties, err := browseForwardReferences(ctx, client, ref.NodeID.NodeID, id.HasProperty, gu.NodeClassVariable)
	if err != nil {
		return domain.CatalogNode{}, fmt.Errorf("browse method %s properties: %w", key.Identifier, err)
	}
	for _, property := range properties {
		if property.BrowseName.Name != "InputArguments" && property.BrowseName.Name != "OutputArguments" {
			continue
		}
		res, err := client.Read(ctx, &gu.ReadRequest{NodesToRead: []*gu.ReadValueID{{
			NodeID: property.NodeID.NodeID, AttributeID: gu.AttributeIDValue,
		}}})
		if err != nil {
			return domain.CatalogNode{}, fmt.Errorf("read method %s %s: %w", key.Identifier, property.BrowseName.Name, err)
		}
		if res == nil || len(res.Results) != 1 {
			return domain.CatalogNode{}, fmt.Errorf("read method %s %s returned an incomplete result", key.Identifier, property.BrowseName.Name)
		}
		args, err := argumentsFromDataValue(res.Results[0])
		if err != nil {
			return domain.CatalogNode{}, fmt.Errorf("method %s %s: %w", key.Identifier, property.BrowseName.Name, err)
		}
		if property.BrowseName.Name == "InputArguments" {
			node.InputArguments = args
		} else {
			node.OutputArguments = args
		}
	}
	return node, nil
}

func stableKey(namespaces map[string]uint16, nodeID *gu.NodeID) (domain.NodeKey, error) {
	if nodeID == nil || nodeID.Type() != gu.NodeIDTypeString {
		return domain.NodeKey{}, fmt.Errorf("only string NodeIds are supported: %v", nodeID)
	}
	var uri string
	for candidate, index := range namespaces {
		if index == nodeID.Namespace() {
			uri = candidate
			break
		}
	}
	if uri == "" {
		return domain.NodeKey{}, fmt.Errorf("namespace index %d has no URI", nodeID.Namespace())
	}
	return domain.NodeKey{NamespaceURI: uri, Identifier: nodeID.StringID()}, nil
}

func dataTypeFromAttribute(value *gu.DataValue) (domain.DataType, error) {
	if err := goodDataValue(value); err != nil {
		return "", err
	}
	nodeID, ok := value.Value.Value().(*gu.NodeID)
	if !ok || nodeID.Namespace() != 0 {
		return "", fmt.Errorf("unsupported data type %T", value.Value.Value())
	}
	return dataTypeFromNumericID(nodeID.IntID())
}

func dataTypeFromNumericID(value uint32) (domain.DataType, error) {
	switch value {
	case id.Boolean:
		return domain.DataTypeBoolean, nil
	case id.UInt32:
		return domain.DataTypeUInt32, nil
	case id.Float:
		return domain.DataTypeFloat, nil
	case id.Double:
		return domain.DataTypeDouble, nil
	case id.String:
		return domain.DataTypeString, nil
	default:
		return "", fmt.Errorf("unsupported OPC data type ns=0;i=%d", value)
	}
}

func int32Attribute(value *gu.DataValue) (int32, error) {
	if err := goodDataValue(value); err != nil {
		return 0, err
	}
	result, ok := value.Value.Value().(int32)
	if !ok {
		return 0, fmt.Errorf("expected Int32, got %T", value.Value.Value())
	}
	return result, nil
}

func byteAttribute(value *gu.DataValue) (byte, error) {
	if err := goodDataValue(value); err != nil {
		return 0, err
	}
	result, ok := value.Value.Value().(uint8)
	if !ok {
		return 0, fmt.Errorf("expected Byte, got %T", value.Value.Value())
	}
	return result, nil
}

func argumentsFromDataValue(value *gu.DataValue) ([]domain.Argument, error) {
	if err := goodDataValue(value); err != nil {
		return nil, err
	}
	var raw []*gu.Argument
	switch args := value.Value.Value().(type) {
	case []*gu.Argument:
		raw = args
	case []gu.Argument:
		raw = make([]*gu.Argument, len(args))
		for i := range args {
			raw[i] = &args[i]
		}
	case []*gu.ExtensionObject:
		raw = make([]*gu.Argument, 0, len(args))
		for _, extension := range args {
			if extension == nil {
				return nil, errors.New("nil Argument extension object")
			}
			switch argument := extension.Value.(type) {
			case *gu.Argument:
				raw = append(raw, argument)
			case gu.Argument:
				copy := argument
				raw = append(raw, &copy)
			default:
				return nil, fmt.Errorf("expected Argument extension object, got %T", extension.Value)
			}
		}
	default:
		return nil, fmt.Errorf("expected Argument[], got %T", value.Value.Value())
	}
	result := make([]domain.Argument, 0, len(raw))
	for _, arg := range raw {
		if arg == nil || arg.DataType == nil || arg.DataType.Namespace() != 0 {
			return nil, errors.New("argument has an unsupported data type")
		}
		dataType, err := dataTypeFromNumericID(arg.DataType.IntID())
		if err != nil {
			return nil, err
		}
		result = append(result, domain.Argument{Name: arg.Name, DataType: dataType, ValueRank: arg.ValueRank})
	}
	return result, nil
}

func goodDataValue(value *gu.DataValue) error {
	if value == nil {
		return errors.New("nil DataValue")
	}
	if value.Status != gu.StatusOK {
		return value.Status
	}
	if value.Value == nil {
		return errors.New("DataValue has no value")
	}
	return nil
}

func sampleFromGopcua(value *gu.DataValue, dataType domain.DataType, epoch uint64) domain.Sample {
	sample := domain.Sample{Type: dataType, StatusCode: domain.StatusBadWaitingInitialData, Epoch: epoch}
	if value == nil {
		return sample
	}
	sample.StatusCode = domain.StatusCode(value.Status)
	sample.SourceTimestamp = value.SourceTimestamp.UTC()
	if value.Value != nil {
		sample.Value = value.Value.Value()
		if sample.Type == "" {
			sample.Type = dataTypeFromVariant(value.Value)
		}
	}
	return sample
}

func dataTypeFromVariant(value *gu.Variant) domain.DataType {
	if value == nil {
		return ""
	}
	switch value.Type() {
	case gu.TypeIDBoolean:
		return domain.DataTypeBoolean
	case gu.TypeIDUint32:
		return domain.DataTypeUInt32
	case gu.TypeIDFloat:
		return domain.DataTypeFloat
	case gu.TypeIDDouble:
		return domain.DataTypeDouble
	case gu.TypeIDString:
		return domain.DataTypeString
	default:
		return ""
	}
}
