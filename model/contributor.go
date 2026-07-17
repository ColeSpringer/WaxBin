package model

// Music contributor roles. These credit the people behind a track beyond its
// performing artist: the composer, the producer, and so on. They share the
// role-tagged item_contributor relation with the audiobook roles (author/narrator/
// translator/editor, defined alongside Book), so a credited person is an artist
// entity with the same dedup and browse machinery. Each maps to a canonical tag key
// for on-disk write-back (see meta.RoleTagKey).
const (
	RoleComposer  ContributorRole = "composer"
	RoleLyricist  ContributorRole = "lyricist"
	RoleConductor ContributorRole = "conductor"
	RolePerformer ContributorRole = "performer"
	RoleRemixer   ContributorRole = "remixer"
	RoleProducer  ContributorRole = "producer"
	RoleEngineer  ContributorRole = "engineer"
	RoleMixer     ContributorRole = "mixer"
	RoleArranger  ContributorRole = "arranger"
	RoleWriter    ContributorRole = "writer"
	RoleDJMixer   ContributorRole = "djmixer"
)

// musicRoles is the set of contributor roles that apply to a music track.
var musicRoles = map[ContributorRole]bool{
	RoleComposer: true, RoleLyricist: true, RoleConductor: true, RolePerformer: true,
	RoleRemixer: true, RoleProducer: true, RoleEngineer: true, RoleMixer: true,
	RoleArranger: true, RoleWriter: true, RoleDJMixer: true,
}

// bookRoles is the set of contributor roles that apply to an audiobook.
var bookRoles = map[ContributorRole]bool{
	RoleAuthor: true, RoleNarrator: true, RoleTranslator: true, RoleEditor: true,
}

// IsMusicRole reports whether r is a music-track contributor role.
func IsMusicRole(r ContributorRole) bool { return musicRoles[r] }

// IsBookRole reports whether r is an audiobook contributor role.
func IsBookRole(r ContributorRole) bool { return bookRoles[r] }

// Valid reports whether r is a known contributor role (music or book).
func (r ContributorRole) Valid() bool { return musicRoles[r] || bookRoles[r] }

// RoleValidForKind reports whether the role applies to the given item kind: music
// roles to tracks, book roles to books.
func RoleValidForKind(r ContributorRole, kind Kind) bool {
	switch kind {
	case KindTrack:
		return IsMusicRole(r)
	case KindBook:
		return IsBookRole(r)
	default:
		return false
	}
}

// CreditField is the field name a role's provenance/lock row uses: "credit.<role>"
// (for example "credit.producer"). It keeps credit locks in the item-scoped
// field_provenance table alongside the scalar fields, namespaced so they never
// collide with a scalar field name.
func CreditField(r ContributorRole) string { return "credit." + string(r) }
