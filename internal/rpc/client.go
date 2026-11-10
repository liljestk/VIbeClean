package rpc

import (
	"context"
	"errors"
	"io"
	"math"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/liljestk/VIbeClean/gen/spacereview/v1"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
)

// maxMessage allows large snapshots, such as a whole-drive capture.
const maxMessage = 256 << 20

// Client is a backend reached over gRPC. It implements service.Backend.
type Client struct {
	conn *grpc.ClientConn
	api  pb.SpaceReviewServiceClient
}

var _ service.Backend = (*Client)(nil)

// Dial connects to a Space Review API server on a loopback address. The
// connection is made lazily, so a missing server shows up on the first call
// as model.ErrUnavailable.
func Dial(address, token string, options ...grpc.DialOption) (*Client, error) {
	if err := checkLoopback(address); err != nil {
		return nil, model.InvalidArgument("%s", err.Error())
	}
	options = append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearer(token)),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessage)),
	}, options...)
	conn, err := grpc.NewClient("passthrough:///"+address, options...)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, api: pb.NewSpaceReviewServiceClient(conn)}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Info identifies the server.
func (c *Client) Info(ctx context.Context) (service.Info, error) {
	reply, err := c.api.GetServerInfo(ctx, &pb.GetServerInfoRequest{})
	if err != nil {
		return service.Info{}, fromStatus(err)
	}
	return service.Info{Version: reply.GetVersion(), IndexPath: reply.GetIndexPath()}, nil
}

// Drives returns the server's fixed-drive readings.
func (c *Client) Drives(ctx context.Context) ([]model.DriveState, error) {
	reply, err := c.api.ListDrives(ctx, &pb.ListDrivesRequest{})
	if err != nil {
		return nil, fromStatus(err)
	}
	return drivesFromPB(reply.GetDrives()), nil
}

// Scopes returns the latest capture of each scope.
func (c *Client) Scopes(ctx context.Context) ([]model.Snapshot, error) {
	reply, err := c.api.ListScopes(ctx, &pb.ListScopesRequest{})
	if err != nil {
		return nil, fromStatus(err)
	}
	scopes := make([]model.Snapshot, 0, len(reply.GetScopes()))
	for _, scope := range reply.GetScopes() {
		scopes = append(scopes, snapshotFromPB(scope))
	}
	return scopes, nil
}

// History returns the saved capture choices for a scope.
func (c *Client) History(ctx context.Context, scope string) ([]model.Snapshot, error) {
	reply, err := c.api.ListHistory(ctx, &pb.ListHistoryRequest{Scope: scope})
	if err != nil {
		return nil, fromStatus(err)
	}
	captures := make([]model.Snapshot, 0, len(reply.GetCaptures()))
	for _, saved := range reply.GetCaptures() {
		captures = append(captures, snapshotFromPB(saved))
	}
	return captures, nil
}

func (c *Client) Inventory(ctx context.Context, q service.InventoryQuery) (service.InventoryPage, error) {
	reply, err := c.api.GetInventory(ctx, inventoryRequest(q))
	if err != nil {
		return service.InventoryPage{}, fromStatus(err)
	}
	page := service.InventoryPage{Total: int(reply.GetTotal())}
	for _, item := range reply.GetItems() {
		if row := recordFromPB(item); row != nil {
			page.Items = append(page.Items, *row)
		}
	}
	return page, nil
}

func inventoryRequest(q service.InventoryQuery) *pb.GetInventoryRequest {
	return &pb.GetInventoryRequest{
		CaptureId: q.CaptureID, Parent: q.Parent, Files: q.Files, Query: q.Query, Within: q.Within,
		Extension: q.Extension, MinimumBytes: q.MinimumBytes, ModifiedSince: millisToPB(q.ModifiedSince),
		Offset: clamp(q.Offset), Limit: clamp(q.Limit), SortColumn: clamp(q.SortColumn), Ascending: q.Ascending,
		Removal: string(q.Removal),
	}
}

func (c *Client) StreamInventory(ctx context.Context, q service.InventoryQuery, emit func(model.Record) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.api.StreamInventory(ctx, &pb.StreamInventoryRequest{Filter: inventoryRequest(q)})
	if err != nil {
		return fromStatus(err)
	}
	for {
		batch, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fromStatus(err)
		}
		for _, raw := range batch.GetItems() {
			if item := recordFromPB(raw); item != nil {
				if err := emit(*item); err != nil {
					return err
				}
			}
		}
	}
}

func (c *Client) Duplicates(ctx context.Context, captureID int64, folder string) (model.DuplicateResult, error) {
	reply, err := c.api.FindDuplicates(ctx, &pb.FindDuplicatesRequest{CaptureId: captureID, Folder: folder})
	if err != nil {
		return model.DuplicateResult{}, fromStatus(err)
	}
	result := model.DuplicateResult{Checked: reply.GetChecked(), Skipped: reply.GetSkipped(), Offline: reply.GetOffline()}
	for _, group := range reply.GetGroups() {
		item := model.DuplicateGroup{Length: group.GetLogicalBytes()}
		for _, copy := range group.GetCopies() {
			if len(copy.GetPaths()) > 0 {
				item.Copies = append(item.Copies, model.DuplicateCopy{
					Paths: copy.GetPaths(), Bytes: copy.GetAllocatedBytes(), Links: copy.GetLinkCount(),
				})
			}
		}
		if len(item.Copies) >= 2 {
			result.Groups = append(result.Groups, item)
		}
	}
	return result, nil
}

// Latest returns the latest capture of a scope.
func (c *Client) Latest(ctx context.Context, q service.LatestQuery) (service.Latest, error) {
	series, err := seriesValue(q.Series)
	if err != nil {
		return service.Latest{}, err
	}
	reply, err := c.api.GetLatest(ctx, &pb.GetLatestRequest{Scope: q.Scope, Series: series, Query: q.Query, Limit: clamp(q.Limit)})
	if err != nil {
		return service.Latest{}, fromStatus(err)
	}
	return service.Latest{Snapshot: snapshotFromPB(reply.GetSnapshot()), RefreshRecommended: reply.GetRefreshRecommended()}, nil
}

// Compare ranks changes between two captures.
func (c *Client) Compare(ctx context.Context, q service.CompareQuery) (service.Comparison, error) {
	series, err := seriesValue(q.Series)
	if err != nil {
		return service.Comparison{}, err
	}
	reply, err := c.api.CompareCaptures(ctx, &pb.CompareCapturesRequest{Scope: q.Scope, Days: clamp(q.Days), Series: series,
		MinimumBytes: q.MinimumBytes, Direction: directionToPB(q.Direction), Query: q.Query, Limit: clamp(q.Limit),
		FromId: q.FromID, ToId: q.ToID})
	if err != nil {
		return service.Comparison{}, fromStatus(err)
	}
	return comparisonFromPB(reply), nil
}

// Capture measures a scope on the server, reporting progress as it arrives.
func (c *Client) Capture(ctx context.Context, req service.CaptureRequest, progress func(service.Progress)) (model.Snapshot, error) {
	stream, err := c.api.Capture(ctx, &pb.CaptureRequest{Scope: req.Scope, Kind: req.Kind, Exclusions: req.Exclusions, Detail: req.Detail})
	if err != nil {
		return model.Snapshot{}, fromStatus(err)
	}
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return model.Snapshot{}, model.Unavailable("the capture ended without a result")
		}
		if err != nil {
			return model.Snapshot{}, fromStatus(err)
		}
		switch event := event.GetEvent().(type) {
		case *pb.CaptureResponse_Progress:
			if progress != nil {
				progress(service.Progress{FilesChecked: event.Progress.GetFilesChecked(), Saving: event.Progress.GetSaving()})
			}
		case *pb.CaptureResponse_Snapshot:
			return snapshotFromPB(event.Snapshot), nil
		}
	}
}

// seriesValue maps a series name, ignoring case, to its enum value.
func seriesValue(name string) (pb.Series, error) {
	if name == "" {
		return pb.Series_SERIES_UNSPECIFIED, nil
	}
	for series, value := range seriesToPB {
		if strings.EqualFold(series, name) {
			return value, nil
		}
	}
	return pb.Series_SERIES_UNSPECIFIED, model.InvalidArgument("unknown series %q", name)
}

// clamp converts a count for the wire, saturating instead of wrapping.
func clamp(value int) int32 {
	return int32(max(math.MinInt32, min(math.MaxInt32, value)))
}
