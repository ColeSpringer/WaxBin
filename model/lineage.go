package model

import "slices"

// SettleIdentities settles, in place, what writes did to items' pids: each absorbed item
// names the item it ended in, through any chain (a cycle stops where it would come back),
// each created item names the item its source ended in, and an item absorbed in the end
// is neither converted nor created. It returns converted without the absorbed.
func SettleIdentities(converted []PID, absorbed, created map[PID]PID) []PID {
	for p, into := range absorbed {
		for range len(absorbed) {
			next, ok := absorbed[into]
			if !ok || next == p {
				break
			}
			into = next
		}
		absorbed[p] = into
	}
	for p, from := range created {
		if _, gone := absorbed[p]; gone {
			delete(created, p)
		} else if into, ok := absorbed[from]; ok {
			created[p] = into
		}
	}
	return slices.DeleteFunc(converted, func(p PID) bool { _, gone := absorbed[p]; return gone })
}
