package rpc

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/liljestk/VIbeClean/gen/spacereview/v1"
	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/service"
)

var seriesToPB = map[string]pb.Series{
	model.Folders:      pb.Series_SERIES_FOLDERS,
	model.Subfolders:   pb.Series_SERIES_SUBFOLDERS,
	model.Candidates:   pb.Series_SERIES_REVIEW_CANDIDATES,
	model.LargeFiles:   pb.Series_SERIES_LARGE_FILES,
	model.DockerTotals: pb.Series_SERIES_DOCKER_TOTALS,
	model.Projects:     pb.Series_SERIES_PROJECTS,
	model.Volumes:      pb.Series_SERIES_VOLUMES,
	model.Containers:   pb.Series_SERIES_CONTAINERS,
	model.Images:       pb.Series_SERIES_IMAGES,
	model.BuildCache:   pb.Series_SERIES_BUILD_CACHE,
	model.Categories:   pb.Series_SERIES_CATEGORIES,
	model.Labeled:      pb.Series_SERIES_APPS_GAMES_PROJECTS,
}

var seriesFromPB = func() map[pb.Series]string {
	reverse := make(map[pb.Series]string, len(seriesToPB))
	for name, value := range seriesToPB {
		reverse[value] = name
	}
	return reverse
}()

func seriesName(series pb.Series) string { return seriesFromPB[series] }

func scopeKindToPB(kind string) pb.ScopeKind {
	switch kind {
	case model.ScopeFiles:
		return pb.ScopeKind_SCOPE_KIND_FILES
	case model.ScopeDocker:
		return pb.ScopeKind_SCOPE_KIND_DOCKER
	}
	return pb.ScopeKind_SCOPE_KIND_UNSPECIFIED
}

func scopeKindFromPB(kind pb.ScopeKind) string {
	switch kind {
	case pb.ScopeKind_SCOPE_KIND_FILES:
		return model.ScopeFiles
	case pb.ScopeKind_SCOPE_KIND_DOCKER:
		return model.ScopeDocker
	}
	return ""
}

func directionFromPB(direction pb.Direction) service.Direction {
	switch direction {
	case pb.Direction_DIRECTION_GROWTH:
		return service.DirectionGrowth
	case pb.Direction_DIRECTION_FREED:
		return service.DirectionFreed
	case pb.Direction_DIRECTION_UNSPECIFIED, pb.Direction_DIRECTION_BOTH:
		return service.DirectionBoth
	}
	return service.Direction(-1)
}

func directionToPB(direction service.Direction) pb.Direction {
	switch direction {
	case service.DirectionGrowth:
		return pb.Direction_DIRECTION_GROWTH
	case service.DirectionFreed:
		return pb.Direction_DIRECTION_FREED
	}
	return pb.Direction_DIRECTION_BOTH
}

func millisToPB(ms int64) *timestamppb.Timestamp {
	if ms == 0 {
		return nil
	}
	return timestamppb.New(time.UnixMilli(ms))
}

func millisFromPB(ts *timestamppb.Timestamp) int64 {
	if ts == nil {
		return 0
	}
	return ts.AsTime().UnixMilli()
}

func drivesToPB(drives []model.DriveState) []*pb.Drive {
	result := make([]*pb.Drive, 0, len(drives))
	for _, d := range drives {
		result = append(result, &pb.Drive{Root: d.Root, VolumeId: d.ID, TotalBytes: d.Total, FreeBytes: d.Free,
			JournalId: d.Journal, NextUsn: d.NextUSN})
	}
	return result
}

func drivesFromPB(drives []*pb.Drive) []model.DriveState {
	var result []model.DriveState
	for _, d := range drives {
		result = append(result, model.DriveState{Root: d.GetRoot(), ID: d.GetVolumeId(), Total: d.GetTotalBytes(),
			Free: d.GetFreeBytes(), Journal: d.GetJournalId(), NextUSN: d.GetNextUsn()})
	}
	return result
}

func coverageToPB(c model.Coverage) *pb.Coverage {
	return &pb.Coverage{ScopeKind: scopeKindToPB(c.ScopeKind), Drives: drivesToPB(c.Drives), Gaps: c.Gaps,
		FilesChecked: c.Checked, RepeatedLinks: c.Shared, OnlineOnly: c.Online, SkippedLinks: c.Links,
		EstimatedDirs: c.Estimated, Subfolders: c.Subfolders, Kind: c.Kind, Engine: c.Engine, Labels: int32(c.Labels)}
}

func coverageFromPB(c *pb.Coverage) model.Coverage {
	return model.Coverage{ScopeKind: scopeKindFromPB(c.GetScopeKind()), Drives: drivesFromPB(c.GetDrives()), Gaps: c.GetGaps(),
		Checked: c.GetFilesChecked(), Shared: c.GetRepeatedLinks(), Online: c.GetOnlineOnly(), Links: c.GetSkippedLinks(),
		Estimated: c.GetEstimatedDirs(), Subfolders: c.GetSubfolders(), Kind: c.GetKind(), Engine: c.GetEngine(),
		Labels: int(c.GetLabels())}
}

func recordToPB(r *model.Record) *pb.Record {
	if r == nil {
		return nil
	}
	return &pb.Record{Series: seriesToPB[r.Series], Key: r.Key, Name: r.Name, Path: r.Path, Kind: r.Kind,
		Bytes: r.Bytes, Files: r.Files, Modified: millisToPB(r.Modified), Directory: r.Directory,
		Activity: r.Activity, Detail: r.Detail, Group: r.Group, Idle: r.Idle,
		ReclaimableBytes: r.Reclaimable, Partial: r.Partial}
}

func recordFromPB(r *pb.Record) *model.Record {
	if r == nil {
		return nil
	}
	return &model.Record{Series: seriesName(r.GetSeries()), Key: r.GetKey(), Name: r.GetName(), Path: r.GetPath(),
		Kind: r.GetKind(), Bytes: r.GetBytes(), Files: r.GetFiles(), Modified: millisFromPB(r.GetModified()),
		Directory: r.GetDirectory(), Activity: r.GetActivity(), Detail: r.GetDetail(), Group: r.GetGroup(),
		Idle: r.GetIdle(), Reclaimable: r.GetReclaimableBytes(), Partial: r.GetPartial()}
}

func snapshotToPB(s model.Snapshot) *pb.Snapshot {
	result := &pb.Snapshot{Id: s.ID, ScopeId: s.Scope, Label: s.Label, CapturedAt: millisToPB(s.Captured),
		Coverage: coverageToPB(s.Coverage), Records: make([]*pb.Record, 0, len(s.Records))}
	for i := range s.Records {
		result.Records = append(result.Records, recordToPB(&s.Records[i]))
	}
	return result
}

func snapshotFromPB(s *pb.Snapshot) model.Snapshot {
	if s == nil {
		return model.Snapshot{}
	}
	result := model.Snapshot{ID: s.GetId(), Scope: s.GetScopeId(), Label: s.GetLabel(),
		Captured: millisFromPB(s.GetCapturedAt()), Coverage: coverageFromPB(s.GetCoverage())}
	for _, r := range s.GetRecords() {
		result.Records = append(result.Records, *recordFromPB(r))
	}
	return result
}

func comparisonToPB(c service.Comparison) *pb.CompareCapturesResponse {
	result := &pb.CompareCapturesResponse{Latest: snapshotToPB(c.Latest), ShortWindow: c.ShortWindow,
		OmittedCoverageChanges: clamp(c.Omitted), Changes: make([]*pb.Change, 0, len(c.Changes))}
	if c.Baseline.ID != 0 {
		result.Baseline = snapshotToPB(c.Baseline)
	}
	for _, d := range c.Drives {
		result.DriveChanges = append(result.DriveChanges, &pb.DriveDelta{Root: d.Root, UsedDeltaBytes: d.UsedDelta, FreeBytes: d.Free})
	}
	for _, change := range c.Changes {
		result.Changes = append(result.Changes, &pb.Change{Series: seriesToPB[change.Series], Before: recordToPB(change.Before),
			After: recordToPB(change.After), DeltaBytes: change.Bytes, MinimumMagnitude: change.Bounded})
	}
	return result
}

func comparisonFromPB(c *pb.CompareCapturesResponse) service.Comparison {
	result := service.Comparison{Comparison: model.Comparison{Latest: snapshotFromPB(c.GetLatest()),
		Baseline: snapshotFromPB(c.GetBaseline()), ShortWindow: c.GetShortWindow(), Omitted: int(c.GetOmittedCoverageChanges())}}
	for _, d := range c.GetDriveChanges() {
		result.Drives = append(result.Drives, model.DriveDelta{Root: d.GetRoot(), UsedDelta: d.GetUsedDeltaBytes(), Free: d.GetFreeBytes()})
	}
	for _, change := range c.GetChanges() {
		result.Changes = append(result.Changes, model.Change{Series: seriesName(change.GetSeries()), Before: recordFromPB(change.GetBefore()),
			After: recordFromPB(change.GetAfter()), Bytes: change.GetDeltaBytes(), Bounded: change.GetMinimumMagnitude()})
	}
	return result
}
