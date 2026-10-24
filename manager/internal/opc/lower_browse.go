package opc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	gu "github.com/gopcua/opcua/ua"
)

type opcReferenceBrowser interface {
	Browse(context.Context, *gu.BrowseRequest) (*gu.BrowseResponse, error)
	BrowseNext(context.Context, *gu.BrowseNextRequest) (*gu.BrowseNextResponse, error)
}

type browseReferenceKey struct {
	nodeID, namespaceURI, referenceTypeID string
	serverIndex                           uint32
	forward                               bool
}

// browseForwardReferences returns an entire reference set or an error, never a
// partial catalog. gopcua's Node.References does not check operation-level Browse
// statuses, so it cannot distinguish an empty folder from a failed operation.
func browseForwardReferences(ctx context.Context, client opcReferenceBrowser, nodeID *gu.NodeID, referenceType uint32, class gu.NodeClass) ([]*gu.ReferenceDescription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pending := make(map[string][]byte)
	remember := func(results []*gu.BrowseResult) {
		for _, result := range results {
			if result != nil && len(result.ContinuationPoint) != 0 {
				pending[string(result.ContinuationPoint)] = append([]byte(nil), result.ContinuationPoint...)
			}
		}
	}
	defer func() {
		if len(pending) == 0 {
			return
		}
		keys := make([]string, 0, len(pending))
		for key := range pending {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		points := make([][]byte, 0, len(keys))
		for _, key := range keys {
			points = append(points, pending[key])
		}
		// Cancellation/timeout must not prevent best-effort cursor release. A
		// failed page remains the discovery error even if release also fails
		// (the server may already have consumed one of these known cursors).
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_, _ = client.BrowseNext(releaseCtx, &gu.BrowseNextRequest{
			ReleaseContinuationPoints: true, ContinuationPoints: points,
		})
	}()

	response, err := client.Browse(ctx, &gu.BrowseRequest{
		RequestedMaxReferencesPerNode: 500,
		NodesToBrowse: []*gu.BrowseDescription{{
			NodeID: nodeID, BrowseDirection: gu.BrowseDirectionForward,
			ReferenceTypeID: gu.NewNumericNodeID(0, referenceType), IncludeSubtypes: true,
			NodeClassMask: uint32(class), ResultMask: uint32(gu.BrowseResultMaskAll),
		}},
	})
	if response != nil {
		remember(response.Results)
	}
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, errors.New("Browse returned a nil response")
	}
	page, err := checkedBrowsePage("Browse", response.ResponseHeader, response.Results)
	if err != nil {
		return nil, err
	}
	var references []*gu.ReferenceDescription
	seen := make(map[browseReferenceKey]struct{})
	for {
		for _, ref := range page.References {
			if ref == nil || ref.NodeID == nil || ref.NodeID.NodeID == nil || ref.BrowseName == nil || ref.ReferenceTypeID == nil {
				return nil, errors.New("Browse returned an incomplete reference")
			}
			if !ref.IsForward || ref.NodeClass&class == 0 {
				return nil, errors.New("Browse returned a reference outside the requested direction or node class")
			}
			key := browseReferenceKey{
				nodeID: ref.NodeID.NodeID.String(), namespaceURI: ref.NodeID.NamespaceURI,
				referenceTypeID: ref.ReferenceTypeID.String(), serverIndex: ref.NodeID.ServerIndex, forward: ref.IsForward,
			}
			if _, exists := seen[key]; exists {
				return nil, fmt.Errorf("Browse repeated reference to %s", ref.NodeID.NodeID)
			}
			seen[key] = struct{}{}
			references = append(references, ref)
		}
		// A successfully consumed page supersedes the preceding cursor. Some
		// servers reuse the same token for successive pages; that is valid as
		// long as the references progress and the final page ends the cursor.
		clear(pending)
		remember([]*gu.BrowseResult{page})
		if len(page.ContinuationPoint) == 0 {
			return references, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next, err := client.BrowseNext(ctx, &gu.BrowseNextRequest{
			ContinuationPoints: [][]byte{append([]byte(nil), page.ContinuationPoint...)},
		})
		if next != nil {
			remember(next.Results)
		}
		if err != nil {
			return nil, err
		}
		if next == nil {
			return nil, errors.New("BrowseNext returned a nil response")
		}
		page, err = checkedBrowsePage("BrowseNext", next.ResponseHeader, next.Results)
		if err != nil {
			return nil, err
		}
	}
}

func checkedBrowsePage(operation string, header *gu.ResponseHeader, results []*gu.BrowseResult) (*gu.BrowseResult, error) {
	if header == nil {
		return nil, fmt.Errorf("%s returned no response header", operation)
	}
	if header.ServiceResult != gu.StatusOK {
		return nil, fmt.Errorf("%s service: %w", operation, header.ServiceResult)
	}
	if len(results) != 1 || results[0] == nil {
		return nil, fmt.Errorf("%s returned an incomplete or unexpected result count", operation)
	}
	if results[0].StatusCode != gu.StatusOK {
		return nil, fmt.Errorf("%s operation: %w", operation, results[0].StatusCode)
	}
	return results[0], nil
}
