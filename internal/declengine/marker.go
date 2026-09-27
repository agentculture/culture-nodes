package declengine

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/jackc/pgx/v5"
)

// MarkerRecord is persisted at dispatch and completed with the provider's
// artifact ID after the action returns. LineageID is loaded from the firing.
type MarkerRecord struct {
	NamespaceID  string
	FiringID     string
	ArtifactKind string
	Nonce        string
	MAC          string
	ArtifactID   string
	LineageID    string
}

type MarkerRejection struct {
	NamespaceID string
	EventID     string
	Outcome     string
	Reason      string
}

// MarkerStore must scope all operations to a namespace. BindArtifact must
// refuse to overwrite an existing, different artifact ID.
type MarkerStore interface {
	SaveMarker(context.Context, MarkerRecord) error
	BindArtifact(context.Context, MarkerRecord, string) error
	LookupMarker(context.Context, string, string, string, string) (MarkerRecord, error)
	RecordMarkerRejection(context.Context, MarkerRejection) error
}

type MarkerService struct {
	key   []byte
	store MarkerStore
}

func NewMarkerService(key []byte, store MarkerStore) (*MarkerService, error) {
	if len(key) < 32 || store == nil {
		return nil, errors.New("declengine: marker key must have at least 32 bytes and store must be set")
	}
	return &MarkerService{key: append([]byte(nil), key...), store: store}, nil
}

func markerField(s string) bool {
	return s != "" && len(s) <= 256 && !strings.Contains(s, ":")
}

func (s *MarkerService) mac(firingID, kind, nonce string) string {
	h := hmac.New(sha256.New, s.key)
	// Length framing avoids ambiguity even if field syntax is expanded later.
	for _, part := range []string{firingID, kind, nonce} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *MarkerService) Mint(ctx context.Context, namespaceID, firingID, artifactKind string) (string, error) {
	if !markerField(namespaceID) || !markerField(firingID) || !markerField(artifactKind) {
		return "", errors.New("declengine: invalid marker identity")
	}
	nonceBytes := make([]byte, 24)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(nonceBytes)
	mac := s.mac(firingID, artifactKind, nonce)
	r := MarkerRecord{NamespaceID: namespaceID, FiringID: firingID, ArtifactKind: artifactKind, Nonce: nonce, MAC: mac}
	if err := s.store.SaveMarker(ctx, r); err != nil {
		return "", err
	}
	return "cn1:" + firingID + ":" + artifactKind + ":" + nonce + ":" + mac, nil
}

type parsedMarker struct{ firingID, kind, nonce, mac string }

func parseMarker(marker string) (parsedMarker, bool) {
	parts := strings.Split(marker, ":")
	if len(parts) != 5 || parts[0] != "cn1" || !markerField(parts[1]) || !markerField(parts[2]) || len(parts[3]) != 48 || len(parts[4]) != 64 {
		return parsedMarker{}, false
	}
	if _, err := hex.DecodeString(parts[3]); err != nil {
		return parsedMarker{}, false
	}
	if _, err := hex.DecodeString(parts[4]); err != nil {
		return parsedMarker{}, false
	}
	return parsedMarker{parts[1], parts[2], parts[3], parts[4]}, true
}

// RecordArtifact binds the action result to the marker before any incoming
// event can inherit it. A marker alone is never enough to establish lineage.
func (s *MarkerService) RecordArtifact(ctx context.Context, namespaceID, marker, artifactID string) error {
	p, ok := parseMarker(marker)
	if !ok || !markerField(namespaceID) || artifactID == "" || !hmac.Equal([]byte(s.mac(p.firingID, p.kind, p.nonce)), []byte(p.mac)) {
		return errors.New("declengine: invalid marker or artifact ID")
	}
	r, err := s.store.LookupMarker(ctx, namespaceID, p.firingID, p.kind, p.nonce)
	if err != nil {
		return err
	}
	if r.FiringID != p.firingID || r.ArtifactKind != p.kind || r.MAC != p.mac || r.NamespaceID != namespaceID {
		return errors.New("declengine: marker was not minted for this firing")
	}
	return s.store.BindArtifact(ctx, r, artifactID)
}

type OriginEvent struct {
	NamespaceID   string
	EventID       string
	Marker        string
	ArtifactKind  string
	ArtifactID    string
	Author        string
	BridgeAccount string
}

// Resolve returns the verified parent firing ID, or empty for a fresh lineage.
// A present but invalid marker produces a durable rejection record.
func (s *MarkerService) Resolve(ctx context.Context, event OriginEvent) (string, error) {
	parent, reason, err := s.verify(ctx, event)
	if err != nil || reason == "" {
		return parent, err
	}
	err = s.store.RecordMarkerRejection(ctx, MarkerRejection{NamespaceID: event.NamespaceID, EventID: event.EventID, Outcome: "marker rejected", Reason: reason})
	return "", err
}

// verify is Resolve without the rejection record: it returns the verified
// parent firing, or the reason the marker does not verify. Engine.StoreIfFrozen
// (task t38) needs exactly this -- in 'before' the declaration engine records
// nothing, so a marker it cannot verify there is simply not its event.
func (s *MarkerService) verify(ctx context.Context, event OriginEvent) (parent, reason string, err error) {
	if event.Marker == "" {
		return "", "", nil
	}
	p, ok := parseMarker(event.Marker)
	if !ok {
		return "", "malformed or unsigned marker", nil
	}
	if !hmac.Equal([]byte(s.mac(p.firingID, p.kind, p.nonce)), []byte(p.mac)) {
		return "", "invalid MAC", nil
	}
	if event.NamespaceID == "" || event.ArtifactID == "" || event.ArtifactKind != p.kind {
		return "", "artifact identity mismatch", nil
	}
	r, err := s.store.LookupMarker(ctx, event.NamespaceID, p.firingID, p.kind, p.nonce)
	if err != nil {
		return "", "", err
	}
	if r.NamespaceID != event.NamespaceID || r.FiringID != p.firingID || r.ArtifactKind != p.kind || r.MAC != p.mac || r.ArtifactID == "" || r.ArtifactID != event.ArtifactID {
		return "", "marker not bound to artifact", nil
	}
	if event.Author != "" && (event.BridgeAccount == "" || event.Author != event.BridgeAccount) {
		return "", "artifact author is not bridge account", nil
	}
	return r.FiringID, "", nil
}

// PostgresMarkerStore persists marker state in the declaration tables. Its
// rejection row uses a reserved declaration identity because rejection occurs
// before declaration matching.
type PostgresMarkerStore struct{ Store *postgres.Store }

func (s PostgresMarkerStore) SaveMarker(ctx context.Context, r MarkerRecord) error {
	_, err := s.Store.Pool().Exec(ctx, `INSERT INTO declaration_minted_markers
		(id,namespace_id,firing_id,artifact_kind,nonce,mac) VALUES($1,$2,$3,$4,$5,$6)`,
		store.NewULID(), r.NamespaceID, r.FiringID, r.ArtifactKind, r.Nonce, r.MAC)
	return err
}

func (s PostgresMarkerStore) BindArtifact(ctx context.Context, r MarkerRecord, artifactID string) error {
	tag, err := s.Store.Pool().Exec(ctx, `UPDATE declaration_minted_markers SET artifact_id=$5
		WHERE namespace_id=$1 AND firing_id=$2 AND artifact_kind=$3 AND nonce=$4
		AND mac=$6 AND (artifact_id IS NULL OR artifact_id=$5)`,
		r.NamespaceID, r.FiringID, r.ArtifactKind, r.Nonce, artifactID, r.MAC)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("declengine: marker already bound to another artifact or missing")
	}
	return nil
}

func (s PostgresMarkerStore) LookupMarker(ctx context.Context, namespaceID, firingID, kind, nonce string) (MarkerRecord, error) {
	var r MarkerRecord
	err := s.Store.Pool().QueryRow(ctx, `SELECT m.namespace_id,m.firing_id,m.artifact_kind,m.nonce,m.mac,
		COALESCE(m.artifact_id,''),f.lineage_id FROM declaration_minted_markers m
		JOIN declaration_firings f ON f.namespace_id=m.namespace_id AND f.id=m.firing_id
		WHERE m.namespace_id=$1 AND m.firing_id=$2 AND m.artifact_kind=$3 AND m.nonce=$4`,
		namespaceID, firingID, kind, nonce).Scan(&r.NamespaceID, &r.FiringID, &r.ArtifactKind,
		&r.Nonce, &r.MAC, &r.ArtifactID, &r.LineageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return MarkerRecord{}, nil
	}
	return r, err
}

func (s PostgresMarkerStore) RecordMarkerRejection(ctx context.Context, r MarkerRejection) error {
	_, err := s.Store.Pool().Exec(ctx, `INSERT INTO declaration_evaluations
		(id,namespace_id,event_id,declaration_id,declaration_version,outcome,reason)
		VALUES($1,$2,$3,'origin-marker','v1',$4,$5)`,
		store.NewULID(), r.NamespaceID, r.EventID, r.Outcome, r.Reason)
	return err
}
