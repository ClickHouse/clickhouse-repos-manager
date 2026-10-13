package store

// SetAfterPart installs a hook that runs after each uploaded part.
func (s *Store) SetAfterPart(fn func(part int32) error) { s.afterPart = fn }
