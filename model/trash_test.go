package model

import (
	"path/filepath"
	"testing"
)

func TestIsTrashNameFolds(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		TrashDirName:     true,
		".WAXBIN-TRASH":  true,
		".Waxbin-Trash":  true,
		".waxbin-trash2": false,
		"waxbin-trash":   false,
		".waxbin-trash.": false,
	} {
		if got := IsTrashName(name); got != want {
			t.Errorf("IsTrashName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestInTrashReadsEveryComponent(t *testing.T) {
	t.Parallel()
	for rel, want := range map[string]bool{
		TrashDirName: true,
		filepath.Join(TrashDirName, "x", "a.mp3"): true,
		filepath.Join("a", ".WAXBIN-TRASH", "b"):  true,
		".":                                       false,
		filepath.Join("Artist", "a.mp3"):          false,
		filepath.Join(TrashDirName+"2", "a.mp3"):  false,
	} {
		if got := InTrash(rel); got != want {
			t.Errorf("InTrash(%q) = %v, want %v", rel, got, want)
		}
	}
}
