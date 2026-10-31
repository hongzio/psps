package store

import "testing"

func TestAnOrderReplacesTheListsLast(t *testing.T) {
	s := open(t)
	if err := s.SetOrder(LiveList, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOrder(LiveList, []string{"b", "c"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOrder(ArchivedList, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	o, err := s.Order()
	if err != nil {
		t.Fatal(err)
	}
	if len(o[LiveList]) != 2 || o[LiveList]["b"] != 0 || o[LiveList]["c"] != 1 || o[ArchivedList]["a"] != 0 {
		t.Fatalf("%v", o)
	}
	if err := s.SetOrder("mine", nil); err == nil {
		t.Fatal("a list that is not one was ordered")
	}
}
