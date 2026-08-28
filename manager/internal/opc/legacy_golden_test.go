package opc

import (
	"reflect"
	"testing"

	awserver "github.com/awcullen/opcua/server"
	awu "github.com/awcullen/opcua/ua"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/platform/pki"
)

// TestLegacyAddressSpaceGolden is a fixed compatibility contract distilled
// from the current Rust server's address-space construction. It deliberately
// does not discover its expectations from the Go server under test.
func TestLegacyAddressSpaceGolden(t *testing.T) {
	pair, err := pki.Ensure(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{}
	server, err := NewUpstreamServer(UpstreamConfig{
		EndpointURL: "opc.tcp://127.0.0.1:4840", CertificateFile: pair.CertificatePath, PrivateKeyFile: pair.PrivateKeyPath,
	}, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	group := domain.Group{ID: "legacy", DefaultSourceID: "source-a", Revision: 1}
	source := domain.Source{ID: "source-a", GroupID: group.ID, Enabled: true, CatalogState: domain.CatalogReady}
	catalog := domain.CatalogUnion{Groups: map[string]map[string]domain.CatalogNode{
		group.ID: {
			"KKS_U32":    legacyVariable("KKS_U32", domain.DataTypeUInt32, false),
			"KKS_BOOL":   legacyVariable("KKS_BOOL", domain.DataTypeBoolean, false),
			"KKS_FLOAT":  legacyVariable("KKS_FLOAT", domain.DataTypeFloat, false),
			"KKS_DOUBLE": legacyVariable("KKS_DOUBLE", domain.DataTypeDouble, false),
			"COM1_Send":  legacyVariable("COM1_Send", domain.DataTypeString, true),
			"COM1_SendCommand": {
				Key:        domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "COM1_SendCommand"},
				BrowseName: "COM1_SendCommand", ParentPath: "Objects/Methods", Class: domain.NodeClassMethod, ValueRank: -1,
				InputArguments:  []domain.Argument{{Name: "HexData", DataType: domain.DataTypeString, ValueRank: -1}},
				OutputArguments: []domain.Argument{{Name: "Result", DataType: domain.DataTypeString, ValueRank: -1}},
			},
		},
	}}
	if err := server.ReconcileCatalog(catalog, []domain.Group{group}, []domain.Source{source}); err != nil {
		t.Fatal(err)
	}

	wantNamespaces := []string{
		"http://opcfoundation.org/UA/",
		ApplicationNamespaceURI,
		BusinessNamespaceURI,
		ManagementNamespaceURI,
	}
	if got := server.NamespaceURIs(); !reflect.DeepEqual(got, wantNamespaces) {
		t.Fatalf("NamespaceArray\n got: %#v\nwant: %#v", got, wantNamespaces)
	}

	type goldenNode struct {
		id, browseName string
		class          awu.NodeClass
		browseNS       uint16
		dataType       awu.NodeID
		valueRank      int32
		accessLevel    byte
		parent         awu.NodeID
		parentRef      awu.NodeID
		typeDefinition awu.NodeID
	}
	golden := []goldenNode{
		{id: "Devices", browseName: "Devices", class: awu.NodeClassObject, browseNS: 2,
			parent: awu.ObjectIDObjectsFolder, parentRef: awu.ReferenceTypeIDOrganizes, typeDefinition: awu.ObjectTypeIDFolderType},
		{id: "Methods", browseName: "Methods", class: awu.NodeClassObject, browseNS: 2,
			parent: awu.ObjectIDObjectsFolder, parentRef: awu.ReferenceTypeIDOrganizes, typeDefinition: awu.ObjectTypeIDBaseObjectType},
		{id: "KKS_U32", browseName: "KKS_U32", class: awu.NodeClassVariable, browseNS: 2,
			dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar, accessLevel: awu.AccessLevelsCurrentRead,
			parent: awu.NewNodeIDString(2, "Devices"), parentRef: awu.ReferenceTypeIDHasComponent, typeDefinition: awu.VariableTypeIDBaseDataVariableType},
		{id: "KKS_BOOL", browseName: "KKS_BOOL", class: awu.NodeClassVariable, browseNS: 2,
			dataType: awu.DataTypeIDBoolean, valueRank: awu.ValueRankScalar, accessLevel: awu.AccessLevelsCurrentRead,
			parent: awu.NewNodeIDString(2, "Devices"), parentRef: awu.ReferenceTypeIDHasComponent, typeDefinition: awu.VariableTypeIDBaseDataVariableType},
		{id: "KKS_FLOAT", browseName: "KKS_FLOAT", class: awu.NodeClassVariable, browseNS: 2,
			dataType: awu.DataTypeIDFloat, valueRank: awu.ValueRankScalar, accessLevel: awu.AccessLevelsCurrentRead,
			parent: awu.NewNodeIDString(2, "Devices"), parentRef: awu.ReferenceTypeIDHasComponent, typeDefinition: awu.VariableTypeIDBaseDataVariableType},
		{id: "KKS_DOUBLE", browseName: "KKS_DOUBLE", class: awu.NodeClassVariable, browseNS: 2,
			dataType: awu.DataTypeIDDouble, valueRank: awu.ValueRankScalar, accessLevel: awu.AccessLevelsCurrentRead,
			parent: awu.NewNodeIDString(2, "Devices"), parentRef: awu.ReferenceTypeIDHasComponent, typeDefinition: awu.VariableTypeIDBaseDataVariableType},
		{id: "COM1_Send", browseName: "COM1_Send", class: awu.NodeClassVariable, browseNS: 2,
			dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar,
			accessLevel: awu.AccessLevelsCurrentRead | awu.AccessLevelsCurrentWrite,
			parent:      awu.NewNodeIDString(2, "Devices"), parentRef: awu.ReferenceTypeIDHasComponent, typeDefinition: awu.VariableTypeIDBaseDataVariableType},
		{id: "COM1_SendCommand", browseName: "COM1_SendCommand", class: awu.NodeClassMethod, browseNS: 2,
			parent: awu.NewNodeIDString(2, "Methods"), parentRef: awu.ReferenceTypeIDHasComponent},
		{id: "COM1_SendCommand_Input", browseName: "InputArguments", class: awu.NodeClassVariable, browseNS: 0,
			dataType: awu.DataTypeIDArgument, valueRank: awu.ValueRankOneDimension, accessLevel: awu.AccessLevelsCurrentRead,
			parent: awu.NewNodeIDString(2, "COM1_SendCommand"), parentRef: awu.ReferenceTypeIDHasProperty, typeDefinition: awu.VariableTypeIDPropertyType},
		{id: "COM1_SendCommand_Output", browseName: "OutputArguments", class: awu.NodeClassVariable, browseNS: 0,
			dataType: awu.DataTypeIDArgument, valueRank: awu.ValueRankOneDimension, accessLevel: awu.AccessLevelsCurrentRead,
			parent: awu.NewNodeIDString(2, "COM1_SendCommand"), parentRef: awu.ReferenceTypeIDHasProperty, typeDefinition: awu.VariableTypeIDPropertyType},
	}

	nm := server.server.NamespaceManager()
	for _, want := range golden {
		t.Run(want.id, func(t *testing.T) {
			id := awu.NewNodeIDString(2, want.id)
			node, ok := nm.FindNode(id)
			if !ok {
				t.Fatalf("missing fixed NodeId %v", id)
			}
			if node.NodeID() != id || node.NodeClass() != want.class {
				t.Fatalf("NodeId/class got=%v/%v want=%v/%v", node.NodeID(), node.NodeClass(), id, want.class)
			}
			if got := node.BrowseName(); got.NamespaceIndex != want.browseNS || got.Name != want.browseName {
				t.Fatalf("BrowseName got=%#v want=ns=%d;%s", got, want.browseNS, want.browseName)
			}
			assertGoldenReference(t, nm.NamespaceUris(), node, want.parentRef, true, want.parent)
			if want.typeDefinition != nil {
				assertGoldenReference(t, nm.NamespaceUris(), node, awu.ReferenceTypeIDHasTypeDefinition, false, want.typeDefinition)
			}
			if variable, ok := node.(*awserver.VariableNode); ok {
				if variable.DataType() != want.dataType || variable.ValueRank() != want.valueRank || variable.AccessLevel() != want.accessLevel {
					t.Fatalf("variable attributes dataType=%v rank=%d access=%#x", variable.DataType(), variable.ValueRank(), variable.AccessLevel())
				}
			}
		})
	}

	method, ok := nm.FindMethod(awu.NewNodeIDString(2, "COM1_SendCommand"))
	if !ok || !method.Executable() || !method.UserExecutable(awu.AnonymousIdentity{}) {
		t.Fatalf("method executable/user executable = %#v", method)
	}
	assertGoldenArgument(t, nm, "COM1_SendCommand_Input", "HexData")
	assertGoldenArgument(t, nm, "COM1_SendCommand_Output", "Result")
}

func legacyVariable(identifier string, dataType domain.DataType, writable bool) domain.CatalogNode {
	return domain.CatalogNode{
		Key:        domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: identifier},
		BrowseName: identifier, ParentPath: "Objects/Devices", Class: domain.NodeClassVariable,
		DataType: dataType, ValueRank: -1, Writable: writable,
	}
}

func assertGoldenReference(t *testing.T, namespaces []string, node awserver.Node, referenceType awu.NodeID, inverse bool, target awu.NodeID) {
	t.Helper()
	for _, reference := range node.References() {
		if reference.ReferenceTypeID == referenceType && reference.IsInverse == inverse && awu.ToNodeID(reference.TargetID, namespaces) == target {
			return
		}
	}
	t.Fatalf("node %v lacks reference type=%v inverse=%v target=%v; refs=%#v", node.NodeID(), referenceType, inverse, target, node.References())
}

func assertGoldenArgument(t *testing.T, nm *awserver.NamespaceManager, identifier, name string) {
	t.Helper()
	variable, ok := nm.FindVariable(awu.NewNodeIDString(2, identifier))
	if !ok {
		t.Fatalf("missing argument node %s", identifier)
	}
	extensions, ok := variable.Value().Value.([]awu.ExtensionObject)
	if !ok || len(extensions) != 1 {
		t.Fatalf("%s value = %#v (%T)", identifier, variable.Value().Value, variable.Value().Value)
	}
	argument, ok := extensions[0].(awu.Argument)
	if !ok {
		t.Fatalf("%s argument = %#v (%T)", identifier, extensions[0], extensions[0])
	}
	if argument.Name != name || argument.DataType != awu.DataTypeIDString || argument.ValueRank != awu.ValueRankScalar {
		t.Fatalf("%s argument got=%#v want name=%s String scalar", identifier, argument, name)
	}
}
