package registry

type Channel struct {
	Name string
	// additional fields wired in Task 9 (subscribers, mutex, etc.)
}

type Registry interface {
	GetOrCreate(name string) *Channel
	Lookup(name string) (*Channel, bool)
	Delete(name string)
	Range(fn func(*Channel) bool)
	Len() int
}
