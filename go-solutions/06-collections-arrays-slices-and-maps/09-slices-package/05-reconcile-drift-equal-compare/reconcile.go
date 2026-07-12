package reconcile

import "slices"

type Rule struct {
	Name     string
	Target   string
	SyncedAt int64
}

func sameIdentity(a, b Rule) bool {
	return a.Name == b.Name && a.Target == b.Target
}

func Reconcile(desired, actual []Rule, apply func([]Rule)) bool {
	if slices.EqualFunc(desired, actual, sameIdentity) {
		return false
	}
	apply(desired)
	return true
}
func Order(a, b []string) int {
	return slices.Compare(a, b)
}
