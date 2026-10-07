package plan

import (
	"context"
	"fmt"
)

// KeepSets holds the IDs of every kept row, in memory, for pass 2. They are
// small: the output is a few tens of megabytes at most.
type KeepSets struct {
	// site holds per-site ID sets by core table role, then blog ID.
	site    map[string]map[int]map[int64]struct{}
	termRel map[int]map[[2]int64]struct{}
	users   map[int64]struct{}
	umeta   map[int64]struct{}
	sites   map[int64]struct{}
}

// keepQueries lists, per core table role, the keep table holding its IDs.
var keepQueries = map[string]string{
	"posts":         `SELECT site, id FROM keep`,
	"postmeta":      `SELECT site, meta_id FROM keep_postmeta`,
	"terms":         `SELECT site, term_id FROM keep_terms`,
	"term_taxonomy": `SELECT site, term_taxonomy_id FROM keep_tt`,
	"termmeta":      `SELECT site, meta_id FROM keep_termmeta`,
	"comments":      `SELECT site, comment_id FROM keep_comments`,
	"commentmeta":   `SELECT site, meta_id FROM keep_commentmeta`,
	"options":       `SELECT site, option_id FROM keep_options`,
}

// LoadKeepSets reads the keep tables into memory.
func (p *Plan) LoadKeepSets(ctx context.Context) (*KeepSets, error) {
	k := &KeepSets{
		site:    map[string]map[int]map[int64]struct{}{},
		termRel: map[int]map[[2]int64]struct{}{},
		users:   map[int64]struct{}{},
		umeta:   map[int64]struct{}{},
		sites:   map[int64]struct{}{},
	}
	for role, q := range keepQueries {
		bySite := map[int]map[int64]struct{}{}
		if err := p.scan(ctx, q, func(site int, id int64) {
			if bySite[site] == nil {
				bySite[site] = map[int64]struct{}{}
			}
			bySite[site][id] = struct{}{}
		}); err != nil {
			return nil, err
		}
		k.site[role] = bySite
	}

	rows, err := p.db.QueryContext(ctx, `SELECT site, object_id, term_taxonomy_id FROM keep_tr`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var site int
		var obj, tt int64
		if err := rows.Scan(&site, &obj, &tt); err != nil {
			rows.Close()
			return nil, err
		}
		if k.termRel[site] == nil {
			k.termRel[site] = map[[2]int64]struct{}{}
		}
		k.termRel[site][[2]int64{obj, tt}] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for q, set := range map[string]map[int64]struct{}{
		`SELECT 0, id FROM keep_users`:          k.users,
		`SELECT 0, umeta_id FROM keep_usermeta`: k.umeta,
		`SELECT 0, blog_id FROM kept_sites`:     k.sites,
	} {
		if err := p.scan(ctx, q, func(_ int, id int64) { set[id] = struct{}{} }); err != nil {
			return nil, err
		}
	}
	return k, nil
}

func (p *Plan) scan(ctx context.Context, q string, fn func(site int, id int64)) error {
	rows, err := p.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("plan: %w\nquery: %s", err, q)
	}
	defer rows.Close()
	for rows.Next() {
		var site int
		var id int64
		if err := rows.Scan(&site, &id); err != nil {
			return err
		}
		fn(site, id)
	}
	return rows.Err()
}

// Has reports whether the row with this ID of a core table role is kept.
// For per-site roles, site is the blog ID; for users and usermeta it is
// ignored.
func (k *KeepSets) Has(role string, site int, id int64) bool {
	switch role {
	case "users":
		_, ok := k.users[id]
		return ok
	case "usermeta":
		_, ok := k.umeta[id]
		return ok
	}
	_, ok := k.site[role][site][id]
	return ok
}

// HasTermRelationship reports whether a term relationship row is kept.
func (k *KeepSets) HasTermRelationship(site int, objectID, termTaxonomyID int64) bool {
	_, ok := k.termRel[site][[2]int64{objectID, termTaxonomyID}]
	return ok
}

// HasPost reports whether a post of a site is kept.
func (k *KeepSets) HasPost(site int, id int64) bool { return k.Has("posts", site, id) }

// HasSite reports whether a site is kept.
func (k *KeepSets) HasSite(blogID int64) bool {
	_, ok := k.sites[blogID]
	return ok
}
