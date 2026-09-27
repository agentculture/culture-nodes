package declengine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type markerMemory struct {
	markers    map[string]MarkerRecord
	rejections []MarkerRejection
}

func (s *markerMemory) SaveMarker(_ context.Context, r MarkerRecord) error {
	if s.markers == nil {
		s.markers = make(map[string]MarkerRecord)
	}
	s.markers[r.Nonce] = r
	return nil
}
func (s *markerMemory) BindArtifact(_ context.Context, marker MarkerRecord, artifactID string) error {
	r := s.markers[marker.Nonce]
	if r.ArtifactID != "" && r.ArtifactID != artifactID {
		return errors.New("already bound")
	}
	r.ArtifactID = artifactID
	s.markers[marker.Nonce] = r
	return nil
}
func (s *markerMemory) LookupMarker(_ context.Context, namespace, firingID, kind, nonce string) (MarkerRecord, error) {
	r := s.markers[nonce]
	if r.NamespaceID != namespace || r.FiringID != firingID || r.ArtifactKind != kind {
		return MarkerRecord{}, nil
	}
	return r, nil
}
func (s *markerMemory) RecordMarkerRejection(_ context.Context, r MarkerRejection) error {
	s.rejections = append(s.rejections, r)
	return nil
}

func TestMarkerLineageResolution(t *testing.T) {
	ctx := context.Background()
	s := &markerMemory{}
	m, err := NewMarkerService([]byte("test control plane secret with sufficient length"), s)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := m.Mint(ctx, "ns", "firing-a", "github.pr")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RecordArtifact(ctx, "ns", marker, "42"); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordArtifact(ctx, "ns", marker, "43"); err == nil {
		t.Fatal("marker rebound to another artifact")
	}
	base := OriginEvent{NamespaceID: "ns", EventID: "event-a", Marker: marker, ArtifactKind: "github.pr", ArtifactID: "42", Author: "bridge", BridgeAccount: "bridge"}
	got, err := m.Resolve(ctx, base)
	if err != nil || got != "firing-a" || len(s.rejections) != 0 {
		t.Fatalf("valid marker: lineage=%q rejections=%v err=%v", got, s.rejections, err)
	}

	cases := []struct {
		name   string
		change func(*OriginEvent)
	}{
		{"copied to another PR", func(e *OriginEvent) { e.ArtifactID = "43" }},
		{"tampered firing id", func(e *OriginEvent) { e.Marker = strings.Replace(e.Marker, "firing-a", "firing-b", 1) }},
		{"unsigned legacy marker", func(e *OriginEvent) { e.Marker = "[culture-nodes:firing firing-a]" }},
		{"wrong author", func(e *OriginEvent) { e.Author = "other" }},
		{"missing bridge account", func(e *OriginEvent) { e.BridgeAccount = "" }},
		{"wrong artifact kind", func(e *OriginEvent) { e.ArtifactKind = "github.comment" }},
		{"wrong namespace", func(e *OriginEvent) { e.NamespaceID = "other" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := base
			e.EventID = tc.name
			tc.change(&e)
			before := len(s.rejections)
			got, err := m.Resolve(ctx, e)
			if err != nil || got != "" || len(s.rejections) != before+1 || s.rejections[before].Outcome != "marker rejected" {
				t.Fatalf("lineage=%q rejections=%v err=%v", got, s.rejections[before:], err)
			}
		})
	}
	unmarked := base
	unmarked.Marker = ""
	unmarked.EventID = "unmarked"
	before := len(s.rejections)
	got, err = m.Resolve(ctx, unmarked)
	if err != nil || got != "" || len(s.rejections) != before {
		t.Fatalf("unmarked: lineage=%q rejections=%v err=%v", got, s.rejections[before:], err)
	}
}

func TestMarkerCannotInheritBeforeArtifactResult(t *testing.T) {
	ctx := context.Background()
	s := &markerMemory{}
	m, err := NewMarkerService([]byte("test control plane secret with sufficient length"), s)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := m.Mint(ctx, "ns", "firing-a", "github.pr")
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Resolve(ctx, OriginEvent{NamespaceID: "ns", EventID: "event", Marker: marker, ArtifactKind: "github.pr", ArtifactID: "42"})
	if err != nil || got != "" || len(s.rejections) != 1 {
		t.Fatalf("unbound marker inherited %q, rejections=%v, err=%v", got, s.rejections, err)
	}
}
