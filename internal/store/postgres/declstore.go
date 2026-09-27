package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/jackc/pgx/v5"
)

type PublishDeclarationInput struct {
	NamespaceID string
	Name        string
	Body        []byte
	Author      string
}

type DeclarationVersion struct {
	ID            string
	NamespaceID   string
	DeclarationID string
	Name          string
	Version       int
	Digest        string
	Body          []byte
	Author        string
	CreatedAt     time.Time
}

// PublishDeclaration serializes publications within a namespace so version
// numbers remain consecutive even when several authors publish at once.
func (s *Store) PublishDeclaration(ctx context.Context, in PublishDeclarationInput) (DeclarationVersion, error) {
	if in.NamespaceID == "" || in.Name == "" || in.Author == "" {
		return DeclarationVersion{}, fmt.Errorf("publish declaration: namespace, name and author are required")
	}
	canonical, err := contracts.CanonicalJSON(json.RawMessage(in.Body))
	if err != nil {
		return DeclarationVersion{}, fmt.Errorf("publish declaration: %w", err)
	}
	digest := contracts.Digest(canonical)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeclarationVersion{}, err
	}
	defer tx.Rollback(ctx)
	var namespaceID string
	if err := tx.QueryRow(ctx, `SELECT id FROM namespaces WHERE id=$1 FOR UPDATE`, in.NamespaceID).Scan(&namespaceID); err != nil {
		return DeclarationVersion{}, err
	}
	var declarationID string
	err = tx.QueryRow(ctx, `SELECT id FROM declarations WHERE namespace_id=$1 AND name=$2`, in.NamespaceID, in.Name).Scan(&declarationID)
	if err == pgx.ErrNoRows {
		declarationID = store.NewULID()
		_, err = tx.Exec(ctx, `INSERT INTO declarations(id,namespace_id,name) VALUES($1,$2,$3)`, declarationID, in.NamespaceID, in.Name)
	}
	if err != nil && err != pgx.ErrNoRows {
		return DeclarationVersion{}, err
	}
	var v DeclarationVersion
	err = tx.QueryRow(ctx, `SELECT id,version,body,author,created_at FROM declaration_versions WHERE declaration_id=$1 AND digest=$2`, declarationID, digest).Scan(&v.ID, &v.Version, &v.Body, &v.Author, &v.CreatedAt)
	if err == nil {
		v.NamespaceID, v.DeclarationID, v.Name, v.Digest = in.NamespaceID, declarationID, in.Name, digest
		return v, tx.Commit(ctx)
	}
	if err != pgx.ErrNoRows {
		return DeclarationVersion{}, err
	}
	v = DeclarationVersion{ID: store.NewULID(), NamespaceID: in.NamespaceID, DeclarationID: declarationID, Name: in.Name, Digest: digest, Body: canonical, Author: in.Author}
	err = tx.QueryRow(ctx, `INSERT INTO declaration_versions(id,namespace_id,declaration_id,version,digest,body,author)
		SELECT $1,$2,$3,coalesce(max(version),0)+1,$4,$5,$6 FROM declaration_versions WHERE declaration_id=$3
		RETURNING version,created_at`, v.ID, v.NamespaceID, v.DeclarationID, v.Digest, v.Body, v.Author).Scan(&v.Version, &v.CreatedAt)
	if err != nil {
		return DeclarationVersion{}, err
	}
	return v, tx.Commit(ctx)
}

// LatestDeclarationVersion returns the newest published version of the
// named declaration in a namespace. internal/declengine/activation.go uses
// it to resolve a fixed activation target at publish time (c33, h64): the
// one-level-deep root-of-trust check needs the target's CURRENT version,
// not whichever version happens to be active.
func (s *Store) LatestDeclarationVersion(ctx context.Context, namespaceID, name string) (DeclarationVersion, error) {
	var v DeclarationVersion
	err := s.pool.QueryRow(ctx, `SELECT v.id,v.namespace_id,v.declaration_id,d.name,v.version,v.digest,v.body,v.author,v.created_at
		FROM declaration_versions v JOIN declarations d ON d.id=v.declaration_id
		WHERE v.namespace_id=$1 AND d.name=$2 ORDER BY v.version DESC LIMIT 1`, namespaceID, name).
		Scan(&v.ID, &v.NamespaceID, &v.DeclarationID, &v.Name, &v.Version, &v.Digest, &v.Body, &v.Author, &v.CreatedAt)
	if err == pgx.ErrNoRows {
		return v, ErrNotFound
	}
	return v, err
}

func (s *Store) GetDeclarationVersion(ctx context.Context, id string) (DeclarationVersion, error) {
	var v DeclarationVersion
	err := s.pool.QueryRow(ctx, `SELECT v.id,v.namespace_id,v.declaration_id,d.name,v.version,v.digest,v.body,v.author,v.created_at
		FROM declaration_versions v JOIN declarations d ON d.id=v.declaration_id WHERE v.id=$1`, id).
		Scan(&v.ID, &v.NamespaceID, &v.DeclarationID, &v.Name, &v.Version, &v.Digest, &v.Body, &v.Author, &v.CreatedAt)
	if err == pgx.ErrNoRows {
		return v, ErrNotFound
	}
	return v, err
}

// ListDeclarations returns the newest version of every declaration in the
// namespace, one row per declaration entity (task t19, #328: the API's
// GET /v1alpha1/declarations list route). Ordered by name for a stable,
// human-readable page -- this is a phase-1 listing, not a paged query.
func (s *Store) ListDeclarations(ctx context.Context, namespaceID string) ([]DeclarationVersion, error) {
	rows, err := s.pool.Query(ctx, `SELECT v.id,v.namespace_id,v.declaration_id,d.name,v.version,v.digest,v.body,v.author,v.created_at
		FROM declaration_versions v
		JOIN declarations d ON d.id = v.declaration_id
		JOIN (SELECT declaration_id, max(version) AS max_version FROM declaration_versions WHERE namespace_id=$1 GROUP BY declaration_id) latest
		  ON latest.declaration_id = v.declaration_id AND latest.max_version = v.version
		WHERE v.namespace_id=$1
		ORDER BY d.name`, namespaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeclarationVersion
	for rows.Next() {
		var v DeclarationVersion
		if err := rows.Scan(&v.ID, &v.NamespaceID, &v.DeclarationID, &v.Name, &v.Version, &v.Digest, &v.Body, &v.Author, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeclarationActivationStatus reports whether declarationID's most recent
// activate/deactivate history entry -- across any of its published
// versions -- is an 'activate', and which version it names. found is false
// when the declaration has never been activated or deactivated at all
// (active is then meaningless, not "false").
func (s *Store) DeclarationActivationStatus(ctx context.Context, namespaceID, declarationID string) (active bool, versionID string, found bool, err error) {
	var kind string
	err = s.pool.QueryRow(ctx, `SELECT h.kind, h.target_version_id FROM declaration_history h
		JOIN declaration_versions v ON v.id = h.target_version_id
		WHERE h.namespace_id=$1 AND v.declaration_id=$2 AND h.kind IN ('activate','deactivate')
		ORDER BY h.seq DESC LIMIT 1`, namespaceID, declarationID).Scan(&kind, &versionID)
	if err == pgx.ErrNoRows {
		return false, "", false, nil
	}
	if err != nil {
		return false, "", false, err
	}
	return kind == "activate", versionID, true, nil
}

// DeclarationName resolves a declaration_id to its name -- links are
// stored by id (LinkDeclarations); the API renders them back into the
// name-addressed shape it uses everywhere else.
func (s *Store) DeclarationName(ctx context.Context, declarationID string) (string, error) {
	var name string
	err := s.pool.QueryRow(ctx, `SELECT name FROM declarations WHERE id=$1`, declarationID).Scan(&name)
	if err == pgx.ErrNoRows {
		return "", ErrNotFound
	}
	return name, err
}

type DeclarationLink struct{ FromDeclarationID, ToDeclarationID, Kind string }

func (s *Store) LinkDeclarations(ctx context.Context, namespaceID, fromID, toID, kind string) error {
	if kind != "must" && kind != "can" {
		return fmt.Errorf("invalid declaration link kind %q", kind)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO declaration_links(id,namespace_id,from_declaration_id,to_declaration_id,kind)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT (namespace_id,from_declaration_id,to_declaration_id,kind) DO NOTHING`, store.NewULID(), namespaceID, fromID, toID, kind)
	return err
}

// ListNamespaceDeclarationLinks returns every declaration link recorded in
// the namespace, from either endpoint (task t20, #328). There is ONE global
// declaration graph (spec c25/c62), so a focus walk (internal/api/declgraph.go)
// loads the whole edge set once and traverses it in memory rather than
// querying per hop.
func (s *Store) ListNamespaceDeclarationLinks(ctx context.Context, namespaceID string) ([]DeclarationLink, error) {
	rows, err := s.pool.Query(ctx, `SELECT from_declaration_id,to_declaration_id,kind FROM declaration_links WHERE namespace_id=$1 ORDER BY kind,from_declaration_id,to_declaration_id`, namespaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var links []DeclarationLink
	for rows.Next() {
		var link DeclarationLink
		if err := rows.Scan(&link.FromDeclarationID, &link.ToDeclarationID, &link.Kind); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

func (s *Store) ListDeclarationLinks(ctx context.Context, namespaceID, fromID string) ([]DeclarationLink, error) {
	rows, err := s.pool.Query(ctx, `SELECT from_declaration_id,to_declaration_id,kind FROM declaration_links WHERE namespace_id=$1 AND from_declaration_id=$2 ORDER BY kind,to_declaration_id`, namespaceID, fromID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var links []DeclarationLink
	for rows.Next() {
		var link DeclarationLink
		if err := rows.Scan(&link.FromDeclarationID, &link.ToDeclarationID, &link.Kind); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

type DeclarationAlias struct {
	ID, NamespaceID, Name string
	ParentAliasID         *string
}

func (s *Store) CreateDeclarationAlias(ctx context.Context, namespaceID, name string) (DeclarationAlias, error) {
	if namespaceID == "" || name == "" {
		return DeclarationAlias{}, fmt.Errorf("create alias: namespace and name are required")
	}
	a := DeclarationAlias{ID: store.NewULID(), NamespaceID: namespaceID, Name: name}
	err := s.pool.QueryRow(ctx, `INSERT INTO declaration_aliases(id,namespace_id,name) VALUES($1,$2,$3) RETURNING id`, a.ID, namespaceID, name).Scan(&a.ID)
	return a, err
}

func (s *Store) AddDeclarationToAlias(ctx context.Context, namespaceID, aliasName, declarationID string) error {
	var inserted string
	err := s.pool.QueryRow(ctx, `INSERT INTO declaration_alias_members(namespace_id,alias_id,declaration_id)
		SELECT $1,a.id,$3 FROM declaration_aliases a WHERE a.namespace_id=$1 AND a.name=$2
		RETURNING alias_id`, namespaceID, aliasName, declarationID).Scan(&inserted)
	if err == pgx.ErrNoRows {
		return ErrNotFound
	}
	return err
}

func (s *Store) ListDeclarationAliasMembers(ctx context.Context, namespaceID, aliasName string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT m.declaration_id FROM declaration_alias_members m
		JOIN declaration_aliases a ON a.id=m.alias_id WHERE a.namespace_id=$1 AND a.name=$2 ORDER BY m.declaration_id`, namespaceID, aliasName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DeclarationAliasDetail is one alias's identity, its nesting (parent
// name, empty at the root), its direct member declaration names, and its
// direct child alias names -- everything GET
// /v1alpha1/declarations/aliases/{name} renders (task t21b, #328, spec
// c31/h23). Both Declarations and Children are ordered by name for a
// stable read; neither descends into nested aliases -- that is
// DeclarationAliasMemberIDsRecursive's job, used by focus instead.
type DeclarationAliasDetail struct {
	ID           string
	NamespaceID  string
	Name         string
	ParentName   string
	Declarations []string
	Children     []string
}

// GetDeclarationAliasDetail resolves an alias by name within a namespace.
// ErrNotFound when no alias of that name exists in this namespace -- an
// alias in another namespace is invisible, same as every other
// namespace-scoped read in this file.
func (s *Store) GetDeclarationAliasDetail(ctx context.Context, namespaceID, name string) (DeclarationAliasDetail, error) {
	out := DeclarationAliasDetail{NamespaceID: namespaceID, Name: name}
	var parentID *string
	err := s.pool.QueryRow(ctx, `SELECT id,parent_alias_id FROM declaration_aliases WHERE namespace_id=$1 AND name=$2`, namespaceID, name).
		Scan(&out.ID, &parentID)
	if err == pgx.ErrNoRows {
		return DeclarationAliasDetail{}, ErrNotFound
	}
	if err != nil {
		return DeclarationAliasDetail{}, err
	}
	if parentID != nil {
		if err := s.pool.QueryRow(ctx, `SELECT name FROM declaration_aliases WHERE namespace_id=$1 AND id=$2`, namespaceID, *parentID).Scan(&out.ParentName); err != nil {
			return DeclarationAliasDetail{}, err
		}
	}

	memberIDs, err := s.ListDeclarationAliasMembers(ctx, namespaceID, name)
	if err != nil {
		return DeclarationAliasDetail{}, err
	}
	names := make([]string, 0, len(memberIDs))
	for _, id := range memberIDs {
		memberName, err := s.DeclarationName(ctx, id)
		if err != nil {
			return DeclarationAliasDetail{}, err
		}
		names = append(names, memberName)
	}
	sort.Strings(names)
	out.Declarations = names

	rows, err := s.pool.Query(ctx, `SELECT name FROM declaration_aliases WHERE namespace_id=$1 AND parent_alias_id=$2 ORDER BY name`, namespaceID, out.ID)
	if err != nil {
		return DeclarationAliasDetail{}, err
	}
	defer rows.Close()
	children := []string{}
	for rows.Next() {
		var childName string
		if err := rows.Scan(&childName); err != nil {
			return DeclarationAliasDetail{}, err
		}
		children = append(children, childName)
	}
	if err := rows.Err(); err != nil {
		return DeclarationAliasDetail{}, err
	}
	out.Children = children
	return out, nil
}

// DeclarationAliasMemberIDsRecursive returns every declaration id that is a
// direct member of the named alias OR a member of any alias nested under
// it, at any depth (task t21b, #328: this is the distance-0 set GET
// .../focus uses when {name} names an alias rather than a declaration --
// c31/h23's "a chain alias resolves by name in every verb that accepts a
// chain"). ErrNotFound when no alias of that name exists in this namespace.
func (s *Store) DeclarationAliasMemberIDsRecursive(ctx context.Context, namespaceID, aliasName string) ([]string, error) {
	var aliasID string
	err := s.pool.QueryRow(ctx, `SELECT id FROM declaration_aliases WHERE namespace_id=$1 AND name=$2`, namespaceID, aliasName).Scan(&aliasID)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE subtree AS (
			SELECT id FROM declaration_aliases WHERE id=$1
			UNION ALL
			SELECT a.id FROM declaration_aliases a JOIN subtree s ON a.parent_alias_id = s.id WHERE a.namespace_id=$2
		)
		SELECT DISTINCT m.declaration_id FROM declaration_alias_members m
		JOIN subtree s ON s.id = m.alias_id
		WHERE m.namespace_id=$2
		ORDER BY m.declaration_id`, aliasID, namespaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type DeclarationHistory struct {
	ID              string
	Sequence        int64
	NamespaceID     string
	Kind            string
	TargetVersionID string
	Actor           string
	SupersedesID    *string
	AliasName       *string
	OldParentName   *string
	NewParentName   *string
	CreatedAt       time.Time
}

func (s *Store) RecordDeclarationActivation(ctx context.Context, namespaceID, targetVersionID, kind, actor, supersedesID string) error {
	if kind != "activate" && kind != "deactivate" {
		return fmt.Errorf("invalid activation kind %q", kind)
	}
	if actor == "" {
		return fmt.Errorf("activation actor is required")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO declaration_history(id,namespace_id,kind,target_version_id,actor,supersedes_id) VALUES($1,$2,$3,$4,$5,$6)`, store.NewULID(), namespaceID, kind, targetVersionID, actor, optionalText(supersedesID))
	return err
}

// MoveDeclarationAlias records both sides of the move in the same transaction
// as its current parent pointer. Locking the namespace serializes cycle checks.
func (s *Store) MoveDeclarationAlias(ctx context.Context, namespaceID, aliasName, parentName, targetVersionID, actor string) error {
	return s.MoveDeclarationAliasWithSupersedes(ctx, namespaceID, aliasName, parentName, targetVersionID, actor, "")
}

// MoveDeclarationAliasWithSupersedes appends a correction that explicitly
// identifies the earlier move; neither history row is rewritten.
func (s *Store) MoveDeclarationAliasWithSupersedes(ctx context.Context, namespaceID, aliasName, parentName, targetVersionID, actor, supersedesID string) error {
	if actor == "" || targetVersionID == "" {
		return fmt.Errorf("alias move: actor and target version are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM namespaces WHERE id=$1 FOR UPDATE`, namespaceID).Scan(&locked); err != nil {
		return err
	}
	var aliasID string
	var oldParentID *string
	if err := tx.QueryRow(ctx, `SELECT id,parent_alias_id FROM declaration_aliases WHERE namespace_id=$1 AND name=$2`, namespaceID, aliasName).Scan(&aliasID, &oldParentID); err != nil {
		return err
	}
	var oldParentName *string
	if oldParentID != nil {
		var n string
		if err := tx.QueryRow(ctx, `SELECT name FROM declaration_aliases WHERE id=$1`, *oldParentID).Scan(&n); err != nil {
			return err
		}
		oldParentName = &n
	}
	var parentID *string
	if parentName != "" {
		var id string
		if err := tx.QueryRow(ctx, `SELECT id FROM declaration_aliases WHERE namespace_id=$1 AND name=$2`, namespaceID, parentName).Scan(&id); err != nil {
			return err
		}
		// Copy before walking: the walk below advances id up the tree, and a
		// pointer to it would store the ROOT as the new parent.
		chosen := id
		parentID = &chosen
		path := []string{aliasName}
		for id != "" {
			var name string
			var next *string
			if err := tx.QueryRow(ctx, `SELECT name,parent_alias_id FROM declaration_aliases WHERE namespace_id=$1 AND id=$2`, namespaceID, id).Scan(&name, &next); err != nil {
				return err
			}
			path = append(path, name)
			if id == aliasID {
				return fmt.Errorf("alias cycle: %s", joinCycle(path))
			}
			if next == nil {
				break
			}
			id = *next
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE declaration_aliases SET parent_alias_id=$1 WHERE id=$2`, parentID, aliasID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO declaration_history(id,namespace_id,kind,target_version_id,actor,alias_name,old_parent_name,new_parent_name,supersedes_id)
		VALUES($1,$2,'alias_move',$3,$4,$5,$6,$7,$8)`, store.NewULID(), namespaceID, targetVersionID, actor, aliasName, oldParentName, optionalText(parentName), optionalText(supersedesID))
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func joinCycle(names []string) string {
	out := ""
	for i, name := range names {
		if i > 0 {
			out += " -> "
		}
		out += name
	}
	return out
}
func optionalText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Store) ListDeclarationHistory(ctx context.Context, namespaceID string) ([]DeclarationHistory, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,seq,namespace_id,kind,target_version_id,actor,supersedes_id,alias_name,old_parent_name,new_parent_name,created_at FROM declaration_history WHERE namespace_id=$1 ORDER BY seq`, namespaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []DeclarationHistory
	for rows.Next() {
		var h DeclarationHistory
		if err := rows.Scan(&h.ID, &h.Sequence, &h.NamespaceID, &h.Kind, &h.TargetVersionID, &h.Actor, &h.SupersedesID, &h.AliasName, &h.OldParentName, &h.NewParentName, &h.CreatedAt); err != nil {
			return nil, err
		}
		history = append(history, h)
	}
	return history, rows.Err()
}
