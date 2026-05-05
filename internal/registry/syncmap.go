package registry

import "sync"

type syncMapReg struct{ m sync.Map }

func NewSyncMap() Registry { return &syncMapReg{} }

func (s *syncMapReg) GetOrCreate(name string) *Channel {
	if v, ok := s.m.Load(name); ok {
		return v.(*Channel)
	}
	c := &Channel{Name: name}
	actual, _ := s.m.LoadOrStore(name, c)
	return actual.(*Channel)
}

func (s *syncMapReg) Lookup(name string) (*Channel, bool) {
	v, ok := s.m.Load(name)
	if !ok {
		return nil, false
	}
	return v.(*Channel), true
}

func (s *syncMapReg) Delete(name string) { s.m.Delete(name) }

func (s *syncMapReg) Range(fn func(*Channel) bool) {
	s.m.Range(func(_, v any) bool { return fn(v.(*Channel)) })
}

func (s *syncMapReg) Len() int {
	n := 0
	s.m.Range(func(_, _ any) bool { n++; return true })
	return n
}
