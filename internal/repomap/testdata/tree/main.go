package tree

import "context"

// Store keeps track of named things.
type Store struct {
	Name string
}

// Load opens a store by name.
func Load(name string) (*Store, error) {
	return &Store{Name: name}, nil
}

// Get returns the store's name.
func (s *Store) Get(ctx context.Context) (string, error) {
	return s.Name, nil
}

// DefaultStore is the name used when none is given.
const DefaultStore = "default"

// Registry holds the open stores.
var Registry = map[string]*Store{}
