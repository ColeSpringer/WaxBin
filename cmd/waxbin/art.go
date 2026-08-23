package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

// artTypeList is the --type vocabulary both art commands accept (model.ArtEntity),
// named once so the resolve and set help texts cannot drift apart.
const artTypeList = "track|album|release_group|artist|genre|episode|podcast|playlist"

// artRungList renders the size ladder for the help text. --size is rounded into this
// vocabulary, so it is published the way the other vocabularies here are, off art.Rungs
// rather than a second copy that can drift from it.
func artRungList() string {
	rungs := art.Rungs()
	parts := make([]string, len(rungs))
	for i, r := range rungs {
		parts[i] = strconv.Itoa(r)
	}
	return strings.Join(parts, " ")
}

// parseArtEntity validates a --type value, naming the vocabulary in the refusal. The
// three art commands share it so the message cannot drift between them; it already had.
func parseArtEntity(op, entType string) (model.ArtEntity, error) {
	et := model.ArtEntity(entType)
	if !et.Valid() {
		return "", waxerr.New(waxerr.CodeInvalid, op,
			fmt.Sprintf("unknown --type %q; valid: %s", entType, artTypeList))
	}
	return et, nil
}

func newArtCmd(g *globals) *cobra.Command {
	var (
		entType string
		role    string
		size    int
		outPath string
	)
	cmd := &cobra.Command{
		Use:   "art PID",
		Short: "Resolve an entity's artwork",
		Long: "Resolves artwork for an entity. The front cover (the default --role) follows the " +
			"fallback chain (track -> album -> release_group -> artist -> genre) to the first level " +
			"that has one; any other role (back|disc|booklet|background) resolves at the entity's " +
			"own level only, as does every role on a playlist, which has no ancestry. With --size, " +
			"returns a thumbnail fitted to a square box, and converts a cover stored in a format " +
			"most clients cannot display even when it already fits. The size is rounded up to the " +
			"nearest of " + artRungList() + ", which is what holds a cover to a few cached " +
			"derivatives rather than one per width ever asked for; a size under the ladder is " +
			"served at 32 and one over it is served as asked. The picture can come back wider " +
			"than the size named, and the rung it was served at is reported. Writes " +
			"the image bytes to --out (or stdout); with --json, reports metadata instead, " +
			"including the chain level that answered, whether an album's cover was derived " +
			"from a member track, and where the picture came from (" + artSourceList +
			") with its provider and fetch URL.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			et, err := parseArtEntity("art", entType)
			if err != nil {
				return err
			}
			r, ok := model.ParseArtRole(role)
			if !ok {
				return waxerr.New(waxerr.CodeInvalid, "art",
					fmt.Sprintf("unknown --role %q; valid: front|back|disc|booklet|background", role))
			}

			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			pid := model.PID(args[0])
			// A sizeless --json reports metadata alone, so it reads through ArtProvenance
			// and never loads the picture. The payload is the same either way: a sizeless
			// resolve always returns the source unscaled, so thumbnail is a literal false
			// and the dimensions are the stored source's. With --size the resolve stays,
			// since only it knows a generated thumbnail's own format and size.
			if g.jsonOut && size <= 0 {
				prov, err := lib.ArtProvenance(ctx(cmd), model.EntityRef{Type: et, PID: pid}, r)
				if err != nil {
					return explainMissingArt(cmd, lib, et, pid, r, err)
				}
				return printArtJSON(cmd, lib, et, pid, r, artView(prov, false, 0))
			}
			blob, err := lib.ResolveArt(ctx(cmd), model.EntityRef{Type: et, PID: pid}, r, size)
			if err != nil {
				return explainMissingArt(cmd, lib, et, pid, r, err)
			}

			if g.jsonOut {
				return printArtJSON(cmd, lib, et, pid, r, artViewFromBlob(blob))
			}
			if outPath != "" {
				if err := os.WriteFile(outPath, blob.Bytes, 0o644); err != nil {
					return waxerr.Wrapf(waxerr.CodeIO, "art", err, "writing %s", outPath)
				}
				rounded := ""
				if blob.Box != size {
					rounded = fmt.Sprintf(", --size %d rounded to a %dpx box", size, blob.Box)
				}
				fmt.Fprintf(out(cmd), "wrote %d bytes (%s %dx%d%s) to %s\n",
					len(blob.Bytes), blob.Format, blob.Width, blob.Height, rounded, outPath)
				return nil
			}
			if _, err := cmd.OutOrStdout().Write(blob.Bytes); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, "art", err)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&entType, "type", "track", "entity type: "+artTypeList)
	f.StringVar(&role, "role", "front", "art role: front|back|disc|booklet|background")
	f.IntVar(&size, "size", 0, "max dimension in px, rounded up to a ladder rung; "+
		"also converts a format most clients cannot display (0 = stored image)")
	f.StringVar(&outPath, "out", "", "write image bytes to this file instead of stdout")
	cmd.AddCommand(newArtRolesCmd(g), newArtSetCmd(g), newArtLockCmd(g, true), newArtLockCmd(g, false))
	return cmd
}

type artRoleView struct {
	Role       string `json:"role"`
	Format     string `json:"format,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	SourceHash string `json:"sourceHash,omitempty"`
	Source     string `json:"source,omitempty"`
	Provider   string `json:"provider,omitempty"`
	SourceURL  string `json:"sourceUrl,omitempty"`
	Updated    int64  `json:"updatedAt,string"` // unix ns; a string, see playStateView
	Locked     bool   `json:"locked"`
}

// artRoleViews renders the slots for --json. Everything but the role, the timestamp,
// and the lock is omitted when empty, so the lock-with-no-artifact row arrives as a
// role that is locked and carries no image rather than one padded with zeroes.
func artRoleViews(roles []model.ArtRoleInfo) []artRoleView {
	out := make([]artRoleView, len(roles))
	for i, r := range roles {
		out[i] = artRoleView{
			Role: string(r.Role), Format: r.Format, Width: r.Width, Height: r.Height,
			SourceHash: r.SourceHash, Source: string(r.Source), Provider: r.Provider,
			SourceURL: r.SourceURL, Updated: r.UpdatedAt, Locked: r.Locked,
		}
	}
	return out
}

// artView renders the `art` command's JSON payload. Both reads produce it, so the shape
// cannot differ between a sizeless resolve and a sized one. box is the ladder rung the
// resolve answered at, which is what tells a caller why it got a picture wider than the
// --size it named and which other sizes would land on these same bytes; it is 0 for a
// sizeless read. A map view marshals an int64 as a bare number, so the timestamp is
// formatted to match the string-encoded ns every other view emits.
func artView(p *model.ArtProvenance, thumbnail bool, box int) map[string]any {
	return map[string]any{
		"format": p.Format, "width": p.Width, "height": p.Height,
		"bytes": p.Size, "sourceHash": p.SourceHash, "thumbnail": thumbnail, "box": box,
		"level": string(p.Level), "derived": p.Derived,
		"source": string(p.Source), "provider": p.Provider,
		"sourceUrl": p.SourceURL, "updatedAt": strconv.FormatInt(p.UpdatedAt, 10),
	}
}

// artViewFromBlob renders a sized resolve's payload. The blob describes the image being
// served, which for a thumbnail is the generated one, so its own format and size go in
// where the source's would. It exists so that mapping lives in one place: the fields it
// reads all hang off the same value, and a call site assembling them by hand turns an
// omitted one into a zero in the payload rather than a build failure.
func artViewFromBlob(b *model.ArtBlob) map[string]any {
	return artView(&model.ArtProvenance{
		Format: b.Format, Width: b.Width, Height: b.Height, Size: len(b.Bytes),
		SourceHash: b.SourceHash, Level: b.Level, Derived: b.Derived,
		Attribution: b.Attribution, UpdatedAt: b.UpdatedAt,
	}, b.Thumbnail, b.Box)
}

// printArtJSON adds the requested entity's own front-cover lock to an art view and
// prints it. locked is that entity's lock, not the lock of whatever chain level
// answered: an album cover derived from a member track says nothing about the album's
// lock. It is absent on a non-front role, which has no lock at all rather than an
// unlocked one.
func printArtJSON(cmd *cobra.Command, lib *waxbin.Library, et model.ArtEntity, pid model.PID,
	r model.ArtRole, view map[string]any) error {
	if r == model.ArtRoleFront {
		locked, err := lib.ArtLocked(ctx(cmd), et, pid)
		if err != nil {
			return err
		}
		view["locked"] = locked
	}
	return printJSON(cmd, view)
}

// newArtRolesCmd lists what an entity holds in its own art slots. It is the only
// read that reports a front-cover lock for every entity type: `art` resolves bytes,
// so a lock with nothing attached reaches it as a not-found, and `provenance` and
// `entity show` cover items and album/artist/release_group alone. It is also the
// only way to see which auxiliary roles an entity carries without fetching each in
// turn.
func newArtRolesCmd(g *globals) *cobra.Command {
	var entType string
	cmd := &cobra.Command{
		Use:   "roles <pid>",
		Short: "List the artwork slots an entity holds",
		Long: "Lists the artwork an entity holds at its own level, with none of the album/" +
			"artist fallback `art` follows: each stored role with its format, size, and where " +
			"the picture came from, plus whether the front cover is locked. A front row with no " +
			"image is a lock with nothing attached, the state that refuses every later `art " +
			"set`; `art unlock` releases it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			et, err := parseArtEntity("art roles", entType)
			if err != nil {
				return err
			}
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			pid := model.PID(args[0])
			roles, err := lib.ArtRoles(ctx(cmd), model.EntityRef{Type: et, PID: pid})
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, struct {
					PID   model.PID     `json:"pid"`
					Type  string        `json:"type"`
					Roles []artRoleView `json:"roles"`
				}{pid, string(et), artRoleViews(roles)})
			}
			w := out(cmd)
			if len(roles) == 0 {
				fmt.Fprintf(w, "%s %s: no artwork of its own, unlocked\n", et, pid)
				return nil
			}
			for _, r := range roles {
				// An empty source hash is the lock-with-no-artifact row, which has no
				// format or dimensions to print and is the reason this command exists.
				desc := "(no image)"
				if r.SourceHash != "" {
					desc = fmt.Sprintf("%s %dx%d", r.Format, r.Width, r.Height)
				}
				lock := ""
				if r.Locked {
					lock = " [locked]"
				}
				fmt.Fprintf(w, "%-11s %s %s%s\n",
					r.Role, desc, attribution(r.Source, r.Provider, r.SourceURL), lock)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&entType, "type", "track", "entity type: "+artTypeList)
	return cmd
}

// explainMissingArt turns a bare not-found from ResolveArt into the diagnosis when the
// entity's front cover is locked with nothing attached. That state is invisible
// otherwise, and it refuses every later `art set`, so the error names it and the way
// out. It does not claim the cover was cleared: `lock <pid> art` on a coverless item
// reaches the same state without one. Any other failure, and any non-front role (which
// has no lock), passes straight through.
func explainMissingArt(cmd *cobra.Command, lib *waxbin.Library, et model.ArtEntity, pid model.PID, r model.ArtRole, err error) error {
	if !waxerr.Is(err, waxerr.CodeNotFound) || r != model.ArtRoleFront {
		return err
	}
	locked, lockErr := lib.ArtLocked(ctx(cmd), et, pid)
	if lockErr != nil || !locked {
		return err
	}
	return waxerr.New(waxerr.CodeNotFound, "art", fmt.Sprintf(
		"no front art for %s %s: the front cover is locked with nothing attached\n(waxbin art unlock %s --type %s)",
		et, pid, pid, et))
}

// newArtLockCmd builds `art lock` or `art unlock`, the lock-only mutation `art set`
// cannot express (that one always writes the cover slot too).
func newArtLockCmd(g *globals, lock bool) *cobra.Command {
	var entType string
	verb, past := "lock", "locked"
	if !lock {
		verb, past = "unlock", "unlocked"
	}
	cmd := &cobra.Command{
		Use:   verb + " <pid>",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " an entity's front cover against automatic writers",
		Long: "Sets or clears the front-cover lock on a track/book item, or on an album/artist/" +
			"release_group/genre/podcast/playlist entity (--type), without touching the cover " +
			"itself. A locked cover survives a scan's re-derive, an enrichment pass, and a " +
			"podcast feed's next image change, and refuses `art set` without --force. Unlocking " +
			"is the way out of a cover cleared with `art set --clear`, which locks by default " +
			"and would otherwise leave every later set refused. Only the front cover has a " +
			"lock; the other roles have no automatic producer to guard against. " +
			"On the default --type track this writes the same lock `waxbin " + verb + " <pid> art` " +
			"does, because an item's art lock has one home; the overlap is deliberate.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			et, err := parseArtEntity("art "+verb, entType)
			if err != nil {
				return err
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()

			pid := model.PID(args[0])
			if err := m.SetArtLock(ctx(cmd), et, pid, lock); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "%s %s front art for %s\n", past, et, pid)
			return nil
		},
	}
	cmd.Flags().StringVar(&entType, "type", "track", "entity type: "+artTypeList)
	return cmd
}

func newArtSetCmd(g *globals) *cobra.Command {
	var (
		entType   string
		role      string
		filePath  string
		clear     bool
		noLock    bool
		keepLock  bool
		force     bool
		writeBack bool
		source    string
		provider  string
		sourceURL string
		format    string
	)
	cmd := &cobra.Command{
		Use:   "set <pid> --file <image>",
		Short: "Set (or clear) an entity's artwork from an image file",
		Long: "Sets artwork for a track/book item, or for an album/artist/release_group/genre/" +
			"podcast/playlist entity (--type), in one role (--role; front is the default, and " +
			"back|disc|booklet|background hold a release's auxiliary images). The image bytes come " +
			"from --file; --clear removes only the named role. A front cover locks the art " +
			"field by default so a scan, an enrichment pass, or a feed sync does not replace " +
			"it; the other roles have no automatic producer and take no lock. That lock " +
			"outlives the cover, so a cleared front cover keeps refusing later sets until " +
			"`art unlock` releases it (--force overrides one set without releasing it). " +
			"--write-back also embeds a front cover into the backing file(s): a track into its " +
			"file, a book into every part, an album across every member track's file. Only the " +
			"front cover has an embedded representation, so --write-back with another role is " +
			"refused. An entity with no file of its own (artist, release_group, genre, podcast, " +
			"episode, playlist) keeps its cover in the catalog, so --write-back does nothing there.\n\n" +
			"The cover is recorded as set by hand unless --source says otherwise, which is what a " +
			"program that fetched the image itself uses to record where it came from.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			et, err := parseArtEntity("art set", entType)
			if err != nil {
				return err
			}
			r, ok := model.ParseArtRole(role)
			if !ok {
				return waxerr.New(waxerr.CodeInvalid, "art set",
					fmt.Sprintf("unknown --role %q; valid: front|back|disc|booklet|background", role))
			}
			attr, err := parseAttribution("art set", source, provider, sourceURL,
				model.Attribution.ValidForArt, artSourceList)
			if err != nil {
				return err
			}
			var raw []byte
			if !clear {
				if filePath == "" {
					return waxerr.New(waxerr.CodeInvalid, "art set", "provide --file or --clear")
				}
				b, err := os.ReadFile(filePath)
				if err != nil {
					return waxerr.Wrapf(waxerr.CodeIO, "art set", err, "reading %s", filePath)
				}
				if len(b) == 0 {
					// Empty bytes are how the store spells a clear, so without this an
					// interrupted download reaching --file deletes the cover and, on the
					// front role, locks the slot against every later set.
					return waxerr.New(waxerr.CodeInvalid, "art set",
						filePath+" is empty; use --clear to remove this role's artwork")
				}
				raw = b
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()

			pid := model.PID(args[0])
			opts := waxbin.ArtEditOptions{
				WriteBack: writeBack, Lock: lockChange(noLock, keepLock), Force: force,
				Source: attr.Source, Provider: attr.Provider, SourceURL: attr.SourceURL,
				Format: format,
			}
			if et == model.ArtTrack {
				err = m.SetItemArt(ctx(cmd), pid, r, raw, opts)
			} else {
				err = m.SetEntityArt(ctx(cmd), et, pid, r, raw, opts)
			}
			if err := surfaceWriteBack(cmd, err); err != nil {
				return err
			}
			if clear {
				// Naming the lock is the point: a cleared front cover is locked by
				// default, which is what refuses every later set, and nothing else
				// reports it. The other roles take no lock, so they get no annotation.
				lockNote := " (locked)"
				switch {
				case r != model.ArtRoleFront:
					lockNote = ""
				case keepLock:
					lockNote = " (lock unchanged)"
				case noLock:
					lockNote = " (unlocked)"
				}
				fmt.Fprintf(out(cmd), "cleared %s %s art for %s%s\n", et, r, pid, lockNote)
			} else {
				fmt.Fprintf(out(cmd), "set %s %s art for %s (%d bytes)\n", et, r, pid, len(raw))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&entType, "type", "track", "entity type: "+artTypeList)
	f.StringVar(&role, "role", "front", "art role: front|back|disc|booklet|background")
	f.StringVar(&filePath, "file", "", "image file to set as this role's artwork")
	f.BoolVar(&clear, "clear", false, "remove this role's artwork instead of setting it")
	f.BoolVar(&noLock, "no-lock", false,
		"unlock the art field (a front cover defaults to locked; an unlocked one may be replaced by a scan, an enrichment pass, or the next feed sync)")
	f.BoolVar(&keepLock, "keep-lock", false, keepLockUsage("the art field"))
	cmd.MarkFlagsMutuallyExclusive("no-lock", "keep-lock")
	f.BoolVar(&force, "force", false, "override a locked art field for this one write")
	f.BoolVar(&writeBack, "write-back", false, "also embed a front cover into the backing file(s) on disk")
	f.StringVar(&source, "source", "", "where the image came from: "+artSourceList+" (default user)")
	f.StringVar(&provider, "provider", "", "service that supplied the image, with --source enrichment")
	f.StringVar(&sourceURL, "source-url", "", "URL the image was fetched from")
	f.StringVar(&format, "format", "",
		"image format for bytes no decoder here recognizes: a token (jxl) or an image media type (image/jxl); ignored when the bytes name themselves")
	return cmd
}
