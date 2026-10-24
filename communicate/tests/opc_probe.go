// Run on 46 from the existing manager Go module (using its pinned client fork).
// This checks isolated loopback test servers only, never deployed device nodes.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
)

func must(ok bool, format string, args ...any) {
	if !ok {
		panic(fmt.Sprintf(format, args...))
	}
}

func main() {
	must(len(os.Args) == 4, "usage: probe ENDPOINT none|secure pending|sequence")
	endpoint, security, mode := os.Args[1], os.Args[2], os.Args[3]
	u, err := url.Parse(endpoint)
	must(err == nil, "endpoint: %v", err)
	port, err := strconv.Atoi(u.Port())
	must(err == nil && u.Scheme == "opc.tcp" && u.Hostname() == "127.0.0.1" && port >= 10000, "isolated loopback endpoint required")
	must(security == "none" || security == "secure", "bad security mode")
	must(mode == "pending" || mode == "sequence" || mode == "report", "bad test mode")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	endpoints, err := opcua.GetEndpoints(ctx, endpoint)
	must(err == nil, "discovery: %v", err)
	policy, messageMode := ua.SecurityPolicyURINone, ua.MessageSecurityModeNone
	if security == "secure" {
		policy, messageMode = ua.SecurityPolicyURIBasic256Sha256, ua.MessageSecurityModeSignAndEncrypt
	}
	ep, err := opcua.SelectEndpoint(endpoints, policy, messageMode)
	must(err == nil, "endpoint selection: %v", err)
	opts := []opcua.Option{opcua.SecurityFromEndpoint(ep, ua.UserTokenTypeAnonymous), opcua.AuthAnonymous(), opcua.AutoReconnect(false), opcua.RequestTimeout(3 * time.Second)}
	if security == "secure" {
		key, e := rsa.GenerateKey(rand.Reader, 2048)
		must(e == nil, "key: %v", e)
		appURI, _ := url.Parse("urn:rcd:communicate:test-client")
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "Communicate isolated probe"},
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageDataEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{appURI}}
		cert, e := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		must(e == nil, "certificate: %v", e)
		opts = append(opts, opcua.Certificate(cert), opcua.PrivateKey(key), opcua.ApplicationURI(appURI.String()))
	}
	client, err := opcua.NewClient(endpoint, opts...)
	must(err == nil, "client: %v", err)
	must(client.Connect(ctx) == nil, "connect failed")
	defer client.Close(context.Background())
	read := func(node *ua.NodeID, attr ua.AttributeID) *ua.DataValue {
		out, e := client.Read(ctx, &ua.ReadRequest{TimestampsToReturn: ua.TimestampsToReturnBoth, NodesToRead: []*ua.ReadValueID{{NodeID: node, AttributeID: attr}}})
		must(e == nil && out != nil && len(out.Results) == 1 && out.Results[0] != nil, "read failed: %v", e)
		return out.Results[0]
	}
	namespaces := read(ua.NewNumericNodeID(0, id.Server_NamespaceArray), ua.AttributeIDValue).Value.Value().([]string)
	var business, diagnostics uint16
	for i, v := range namespaces {
		if v == "urn:SerialServer" {
			business = uint16(i)
		}
		if v == "urn:RcdCommunicate:diagnostics" {
			diagnostics = uint16(i)
		}
	}
	must(business > 0 && diagnostics > 0, "namespace missing")
	identity := read(ua.NewStringNodeID(diagnostics, "Diagnostics/InstanceId"), ua.AttributeIDValue)
	expectedIdentity := "communicate-example"
	pointCount := 2
	if mode == "report" {
		expectedIdentity, pointCount = "unit1-receiver", 58
	}
	must(identity.Status == ua.StatusOK && identity.Value.Value() == expectedIdentity, "not the isolated example server")
	browse, err := client.Browse(ctx, &ua.BrowseRequest{NodesToBrowse: []*ua.BrowseDescription{{NodeID: ua.NewStringNodeID(business, "Devices"), BrowseDirection: ua.BrowseDirectionForward,
		ReferenceTypeID: ua.NewNumericNodeID(0, id.HasComponent), IncludeSubtypes: true, NodeClassMask: uint32(ua.NodeClassVariable), ResultMask: uint32(ua.BrowseResultMaskAll)}}})
	must(err == nil && browse != nil && len(browse.Results) == 1 && browse.Results[0].StatusCode == ua.StatusOK && len(browse.Results[0].References) == pointCount, "browse point table: %v", err)
	node := ua.NewStringNodeID(business, "COMM_EXAMPLE_VALUE")
	expectedType := uint32(id.Double)
	if mode == "report" {
		node = ua.NewStringNodeID(business, "DATA.Buildings.91UBH.Fire")
		expectedType = id.UInt32
		parent := ua.NewStringNodeID(diagnostics, "Diagnostics/Ports/sync")
		method := ua.NewStringNodeID(diagnostics, "Diagnostics/Ports/sync/SetTransmitSnapshot")
		statusNode := ua.NewStringNodeID(diagnostics, "Diagnostics/Ports/sync/Synchronization")
		var status map[string]any
		must(json.Unmarshal([]byte(read(statusNode, ua.AttributeIDValue).Value.Value().(string)), &status) == nil, "sync status")
		revision := uint64(status["revision"].(float64))
		payload := make([]byte, 1156)
		payload[0] = 1
		request, _ := json.Marshal(map[string]any{"revision": revision, "payload_hex": fmt.Sprintf("%X", payload)})
		inputNode := ua.NewStringNodeID(business, "DATA.Header.detectorTemp3")
		fireInput := ua.NewStringNodeID(business, "DATA.Buildings.11UBE.Fire")
		description := read(fireInput, ua.AttributeIDDescription)
		must(description.Status == ua.StatusOK && description.Value != nil, "enum description missing")
		must(read(inputNode, ua.AttributeIDAccessLevel).Value.Value() == byte(3), "TX input is not writable")
		for _, v := range []any{uint32(7), uint32(7), uint32(256), "7"} {
			response, e := client.Write(ctx, &ua.WriteRequest{NodesToWrite: []*ua.WriteValue{{NodeID: inputNode, AttributeID: ua.AttributeIDValue,
				Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(v)}}}})
			must(e == nil && response != nil && len(response.Results) == 1, "TX write transport: %v", e)
			want := v == uint32(7)
			must((response.Results[0] == ua.StatusOK) == want, "TX validation: %v %v", v, response.Results)
		}
		must(read(inputNode, ua.AttributeIDValue).Value.Value() == uint32(7), "TX readback")
		must(json.Unmarshal([]byte(read(statusNode, ua.AttributeIDValue).Value.Value().(string)), &status) == nil && status["snapshot_fresh"] == false, "partial input must not be transmitted")
		for _, v := range []any{uint32(2), uint32(0), uint32(9), "2"} {
			response, e := client.Write(ctx, &ua.WriteRequest{NodesToWrite: []*ua.WriteValue{{NodeID: fireInput, AttributeID: ua.AttributeIDValue, Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(v)}}}})
			must(e == nil && len(response.Results) == 1 && (response.Results[0] == ua.StatusOK) == (v == uint32(2)), "enum write: %v %v %v", v, response, e)
		}
		must(read(fireInput, ua.AttributeIDValue).Value.Value() == uint32(2), "invalid enum changed last accepted value")
		// A complete raw diagnostic snapshot enters atomic mode. Test after
		// individual writes; later field writes must then be rejected.
		must(json.Unmarshal([]byte(read(statusNode, ua.AttributeIDValue).Value.Value().(string)), &status) == nil, "current revision")
		request, _ = json.Marshal(map[string]any{"revision": uint64(status["revision"].(float64)), "payload_hex": fmt.Sprintf("%X", payload)})
		for attempt := 0; attempt < 2; attempt++ {
			response, e := client.Call(ctx, &ua.CallMethodRequest{ObjectID: parent, MethodID: method, InputArguments: []*ua.Variant{ua.MustVariant(string(request))}})
			must(e == nil && response.StatusCode == ua.StatusOK, "snapshot method: %v", e)
			var result map[string]any
			must(json.Unmarshal([]byte(response.OutputArguments[0].Value().(string)), &result) == nil && result["ok"] == (attempt == 0), "snapshot CAS")
		}
		response, e := client.Write(ctx, &ua.WriteRequest{NodesToWrite: []*ua.WriteValue{{NodeID: fireInput, AttributeID: ua.AttributeIDValue, Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(uint32(1))}}}})
		must(e == nil && response.Results[0] != ua.StatusOK, "atomic mode permitted mixed field write")
	}
	must(read(node, ua.AttributeIDDataType).Value.Value().(*ua.NodeID).IntID() == expectedType, "wrong type")
	must(read(node, ua.AttributeIDValueRank).Value.Value() == int32(-1), "not scalar")
	must(read(node, ua.AttributeIDAccessLevel).Value.Value() == byte(1), "not read-only")
	if mode == "pending" {
		value := read(node, ua.AttributeIDValue)
		must(value.Status == ua.StatusBadOutOfService && value.Value.Value() == float64(0) && value.SourceTimestamp.IsZero(), "pending value pretends to be sampled: %+v", value)
	}
	// A write to the known read-only software fixture must be rejected. There
	// is no command node or serial handle in either test server.
	written, err := client.Write(ctx, &ua.WriteRequest{NodesToWrite: []*ua.WriteValue{{NodeID: node, AttributeID: ua.AttributeIDValue, Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(float64(99))}}}})
	must(err == nil && written != nil && len(written.Results) == 1 && written.Results[0] != ua.StatusOK, "read-only write was accepted: %v", err)
	updates := make(chan *opcua.PublishNotificationData, 16)
	sub, err := client.Subscribe(ctx, &opcua.SubscriptionParameters{Interval: 100 * time.Millisecond}, updates)
	must(err == nil, "subscribe: %v", err)
	defer sub.Cancel(context.Background())
	monitored, err := sub.Monitor(ctx, ua.TimestampsToReturnBoth, opcua.NewMonitoredItemCreateRequestWithDefaults(node, ua.AttributeIDValue, 1))
	must(err == nil && monitored != nil && len(monitored.Results) == 1 && monitored.Results[0].StatusCode == ua.StatusOK, "monitor: %v", err)
	seenGood, seenBad, complete := false, false, false
	var sourceTime time.Time
	notifications := 0
	for !complete {
		select {
		case <-ctx.Done():
			panic("notification sequence timed out")
		case event := <-updates:
			must(event != nil && event.Error == nil, "notification failed: %+v", event)
			change, ok := event.Value.(*ua.DataChangeNotification)
			if !ok {
				continue
			}
			for _, item := range change.MonitoredItems {
				value := item.Value
				if mode == "report" {
					fmt.Fprintf(os.Stderr, "notification status=%s value=%v source=%s\n", value.Status, value.Value.Value(), value.SourceTimestamp)
				}
				notifications++
				expectedValue := any(float64(0))
				if mode == "report" {
					expectedValue = uint32(0)
					if value.Status == ua.StatusOK {
						expectedValue = uint32(3)
					}
				}
				must(value.Value.Value() == expectedValue && !value.ServerTimestamp.IsZero(), "wrong value/server timestamp")
				if mode == "pending" {
					must(value.Status == ua.StatusBadOutOfService, "pending quality changed")
					complete = true
					break
				}
				if value.Status == ua.StatusOK {
					must(!value.SourceTimestamp.IsZero(), "good response lost source timestamp")
					if seenBad {
						complete = true
					}
					seenGood, sourceTime = true, value.SourceTimestamp
				} else if value.Status == ua.StatusBadNoCommunication && seenGood {
					must(value.SourceTimestamp.Equal(sourceTime), "failure forged source time")
					seenBad = true
				}
			}
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"passed": true, "security": security, "mode": mode, "notifications": notifications,
		"business_namespace": business, "point_count": pointCount, "read_only_enforced": true, "hardware_used": false})
}
