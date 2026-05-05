package registry

import "testing"

func TestSyncMap(t *testing.T) { runRegistryTests(t, NewSyncMap) }
