package registry

import "testing"

func TestSharded(t *testing.T) { runRegistryTests(t, NewSharded) }
