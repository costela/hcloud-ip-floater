package stringset

// StringSet is a very thin wrapper around a map with string keys and empty values
type StringSet map[string]struct{}

func (s StringSet) Add(i string) {
	s[i] = struct{}{}
}

func (s StringSet) Has(i string) bool {
	_, found := s[i]
	return found
}

// Diff returns the difference between the current StringSet and `other`. I.e.: `s \ other`
func (s StringSet) Diff(other StringSet) StringSet {
	res := make(StringSet)

	for i := range s {
		if _, found := other[i]; !found {
			res.Add(i)
		}
	}

	return res
}
