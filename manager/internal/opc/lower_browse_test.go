package opc

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gopcua/opcua/id"
	gu "github.com/gopcua/opcua/ua"
)

type browseNextStep struct {
	response *gu.BrowseNextResponse
	err      error
}

type scriptedReferenceBrowser struct {
	response       *gu.BrowseResponse
	err            error
	next           []browseNextStep
	browseHook     func()
	browseRequest  *gu.BrowseRequest
	nextCursors    []string
	released       []string
	releaseErr     error
	releaseContext error
	releaseTimeout time.Duration
}

func (b *scriptedReferenceBrowser) Browse(_ context.Context, request *gu.BrowseRequest) (*gu.BrowseResponse, error) {
	b.browseRequest = request
	if b.browseHook != nil {
		b.browseHook()
	}
	return b.response, b.err
}

func (b *scriptedReferenceBrowser) BrowseNext(ctx context.Context, request *gu.BrowseNextRequest) (*gu.BrowseNextResponse, error) {
	if request.ReleaseContinuationPoints {
		for _, point := range request.ContinuationPoints {
			b.released = append(b.released, string(point))
		}
		b.releaseContext = ctx.Err()
		if deadline, ok := ctx.Deadline(); ok {
			b.releaseTimeout = time.Until(deadline)
		}
		return &gu.BrowseNextResponse{ResponseHeader: &gu.ResponseHeader{}}, b.releaseErr
	}
	if len(request.ContinuationPoints) != 1 {
		return nil, errors.New("fixture expected exactly one continuation point")
	}
	b.nextCursors = append(b.nextCursors, string(request.ContinuationPoints[0]))
	if len(b.next) == 0 {
		return nil, errors.New("unexpected BrowseNext")
	}
	step := b.next[0]
	b.next = b.next[1:]
	return step.response, step.err
}

func browseTestReference(name string) *gu.ReferenceDescription {
	return &gu.ReferenceDescription{
		ReferenceTypeID: gu.NewNumericNodeID(0, id.HasComponent), IsForward: true,
		NodeID:     gu.NewExpandedNodeID(gu.NewStringNodeID(2, name), "", 0),
		BrowseName: &gu.QualifiedName{NamespaceIndex: 2, Name: name}, NodeClass: gu.NodeClassVariable,
	}
}

func browseTestPage(cursor string, names ...string) *gu.BrowseResult {
	page := &gu.BrowseResult{ContinuationPoint: []byte(cursor)}
	for _, name := range names {
		page.References = append(page.References, browseTestReference(name))
	}
	return page
}

func browseTestResponse(pages ...*gu.BrowseResult) *gu.BrowseResponse {
	return &gu.BrowseResponse{ResponseHeader: &gu.ResponseHeader{}, Results: pages}
}

func browseNextTestResponse(pages ...*gu.BrowseResult) *gu.BrowseNextResponse {
	return &gu.BrowseNextResponse{ResponseHeader: &gu.ResponseHeader{}, Results: pages}
}

func TestBrowseForwardReferencesCollectsAllPagesAndAllowsReusedCursor(t *testing.T) {
	browser := &scriptedReferenceBrowser{
		response: browseTestResponse(browseTestPage("cursor", "A")),
		next: []browseNextStep{
			{response: browseNextTestResponse(browseTestPage("cursor", "B"))},
			{response: browseNextTestResponse(browseTestPage("", "C"))},
		},
	}
	refs, err := browseForwardReferences(context.Background(), browser, gu.NewStringNodeID(2, "Devices"), id.HasComponent, gu.NodeClassVariable)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ref := range refs {
		names = append(names, ref.BrowseName.Name)
	}
	if !reflect.DeepEqual(names, []string{"A", "B", "C"}) || !reflect.DeepEqual(browser.nextCursors, []string{"cursor", "cursor"}) {
		t.Fatalf("references=%v cursors=%v", names, browser.nextCursors)
	}
	if len(browser.released) != 0 {
		t.Fatalf("completed cursors must not be released again: %v", browser.released)
	}
	request := browser.browseRequest
	if request.RequestedMaxReferencesPerNode != 500 || len(request.NodesToBrowse) != 1 || request.NodesToBrowse[0].BrowseDirection != gu.BrowseDirectionForward || request.NodesToBrowse[0].ResultMask != uint32(gu.BrowseResultMaskAll) {
		t.Fatalf("unexpected browse request: %#v", request)
	}
}

func TestBrowseForwardReferencesAcceptsEmptyCatalogAndEmptyIntermediatePage(t *testing.T) {
	t.Run("empty folder", func(t *testing.T) {
		browser := &scriptedReferenceBrowser{response: browseTestResponse(browseTestPage(""))}
		refs, err := browseForwardReferences(context.Background(), browser, gu.NewStringNodeID(2, "Devices"), id.HasComponent, gu.NodeClassVariable)
		if err != nil || len(refs) != 0 || len(browser.nextCursors) != 0 || len(browser.released) != 0 {
			t.Fatalf("empty folder: refs=%v err=%v browser=%#v", refs, err, browser)
		}
	})
	t.Run("empty intermediate page", func(t *testing.T) {
		browser := &scriptedReferenceBrowser{
			response: browseTestResponse(browseTestPage("cursor")),
			next:     []browseNextStep{{response: browseNextTestResponse(browseTestPage("", "A"))}},
		}
		refs, err := browseForwardReferences(context.Background(), browser, gu.NewStringNodeID(2, "Devices"), id.HasComponent, gu.NodeClassVariable)
		if err != nil || len(refs) != 1 || refs[0].BrowseName.Name != "A" || len(browser.released) != 0 {
			t.Fatalf("empty intermediate page: refs=%v err=%v browser=%#v", refs, err, browser)
		}
	})
}

func TestBrowseForwardReferencesRejectsFailedOrIncompleteBrowse(t *testing.T) {
	transport := errors.New("browse transport failed")
	for _, test := range []struct {
		name     string
		response *gu.BrowseResponse
		err      error
		status   error
		released []string
	}{
		{name: "transport", err: transport, status: transport},
		{name: "nil response"},
		{name: "nil header", response: &gu.BrowseResponse{Results: []*gu.BrowseResult{browseTestPage("cursor")}}, released: []string{"cursor"}},
		{name: "service failure", response: &gu.BrowseResponse{ResponseHeader: &gu.ResponseHeader{ServiceResult: gu.StatusBadInternalError}}, status: gu.StatusBadInternalError},
		{name: "operation failure", response: browseTestResponse(&gu.BrowseResult{StatusCode: gu.StatusBadNodeIDUnknown}), status: gu.StatusBadNodeIDUnknown},
		{name: "missing result", response: browseTestResponse()},
		{name: "nil result", response: browseTestResponse(nil)},
		{name: "extra results", response: browseTestResponse(browseTestPage("cursor-a"), browseTestPage("cursor-b")), released: []string{"cursor-a", "cursor-b"}},
		{name: "nil reference", response: browseTestResponse(&gu.BrowseResult{ContinuationPoint: []byte("cursor"), References: []*gu.ReferenceDescription{nil}}), released: []string{"cursor"}},
		{name: "incomplete reference", response: browseTestResponse(&gu.BrowseResult{References: []*gu.ReferenceDescription{{}}})},
		{name: "transport with allocated cursor", response: browseTestResponse(browseTestPage("cursor")), err: transport, status: transport, released: []string{"cursor"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			browser := &scriptedReferenceBrowser{response: test.response, err: test.err}
			refs, err := browseForwardReferences(context.Background(), browser, gu.NewStringNodeID(2, "Devices"), id.HasComponent, gu.NodeClassVariable)
			if err == nil || refs != nil || (test.status != nil && !errors.Is(err, test.status)) {
				t.Fatalf("failed Browse accepted: refs=%v err=%v want=%v", refs, err, test.status)
			}
			if !reflect.DeepEqual(browser.released, test.released) {
				t.Fatalf("released=%v want=%v", browser.released, test.released)
			}
		})
	}
}

func TestBrowseForwardReferencesRejectsFailedOrIncompleteContinuation(t *testing.T) {
	transport := errors.New("next page transport failed")
	for _, test := range []struct {
		name     string
		step     browseNextStep
		status   error
		released []string
	}{
		{name: "transport", step: browseNextStep{err: transport}, status: transport, released: []string{"cursor-1"}},
		{name: "nil response", released: []string{"cursor-1"}},
		{name: "nil header", step: browseNextStep{response: &gu.BrowseNextResponse{Results: []*gu.BrowseResult{browseTestPage("cursor-2")}}}, released: []string{"cursor-1", "cursor-2"}},
		{name: "service failure", step: browseNextStep{response: &gu.BrowseNextResponse{ResponseHeader: &gu.ResponseHeader{ServiceResult: gu.StatusBadInternalError}}}, status: gu.StatusBadInternalError, released: []string{"cursor-1"}},
		{name: "operation failure", step: browseNextStep{response: browseNextTestResponse(&gu.BrowseResult{StatusCode: gu.StatusBadContinuationPointInvalid, ContinuationPoint: []byte("cursor-2")})}, status: gu.StatusBadContinuationPointInvalid, released: []string{"cursor-1", "cursor-2"}},
		{name: "missing result", step: browseNextStep{response: browseNextTestResponse()}, released: []string{"cursor-1"}},
		{name: "nil result", step: browseNextStep{response: browseNextTestResponse(nil)}, released: []string{"cursor-1"}},
		{name: "extra results", step: browseNextStep{response: browseNextTestResponse(browseTestPage("cursor-2"), browseTestPage("cursor-3"))}, released: []string{"cursor-1", "cursor-2", "cursor-3"}},
		{name: "repeated reference", step: browseNextStep{response: browseNextTestResponse(browseTestPage("cursor-1", "A"))}, released: []string{"cursor-1"}},
		{name: "nil reference", step: browseNextStep{response: browseNextTestResponse(&gu.BrowseResult{References: []*gu.ReferenceDescription{nil}})}, released: []string{"cursor-1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			browser := &scriptedReferenceBrowser{response: browseTestResponse(browseTestPage("cursor-1", "A")), next: []browseNextStep{test.step}}
			refs, err := browseForwardReferences(context.Background(), browser, gu.NewStringNodeID(2, "Devices"), id.HasComponent, gu.NodeClassVariable)
			if err == nil || refs != nil || (test.status != nil && !errors.Is(err, test.status)) {
				t.Fatalf("partial catalog accepted: refs=%v err=%v want=%v", refs, err, test.status)
			}
			if !reflect.DeepEqual(browser.released, test.released) {
				t.Fatalf("released=%v want=%v", browser.released, test.released)
			}
		})
	}
}

func TestBrowseForwardReferencesReleasesCursorsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	browser := &scriptedReferenceBrowser{response: browseTestResponse(browseTestPage("cursor", "A")), browseHook: cancel}
	refs, err := browseForwardReferences(ctx, browser, gu.NewStringNodeID(2, "Devices"), id.HasComponent, gu.NodeClassVariable)
	if refs != nil || !errors.Is(err, context.Canceled) || !reflect.DeepEqual(browser.released, []string{"cursor"}) {
		t.Fatalf("refs=%v err=%v released=%v", refs, err, browser.released)
	}
	if browser.releaseContext != nil || browser.releaseTimeout <= 0 || browser.releaseTimeout > 2*time.Second {
		t.Fatalf("release must use a fresh, bounded context: err=%v timeout=%s", browser.releaseContext, browser.releaseTimeout)
	}
}

func TestBrowseForwardReferencesCanceledBeforeBrowseDoesNotAllocateCursor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	browser := &scriptedReferenceBrowser{}
	refs, err := browseForwardReferences(ctx, browser, gu.NewStringNodeID(2, "Devices"), id.HasComponent, gu.NodeClassVariable)
	if refs != nil || !errors.Is(err, context.Canceled) || browser.browseRequest != nil || len(browser.released) != 0 {
		t.Fatalf("canceled browse: refs=%v err=%v browser=%#v", refs, err, browser)
	}
}

func TestBrowseForwardReferencesReleaseFailureDoesNotHideDiscoveryError(t *testing.T) {
	browser := &scriptedReferenceBrowser{
		response: browseTestResponse(browseTestPage("cursor", "A")),
		next: []browseNextStep{{response: browseNextTestResponse(&gu.BrowseResult{
			StatusCode: gu.StatusBadContinuationPointInvalid,
		})}},
		releaseErr: errors.New("release failed"),
	}
	refs, err := browseForwardReferences(context.Background(), browser, gu.NewStringNodeID(2, "Devices"), id.HasComponent, gu.NodeClassVariable)
	if refs != nil || !errors.Is(err, gu.StatusBadContinuationPointInvalid) || len(browser.released) != 1 {
		t.Fatalf("refs=%v err=%v released=%v", refs, err, browser.released)
	}
}
