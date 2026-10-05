package link

import (
	"errors"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// bulkMirror answers LibrariesByLink's whole question in one call and refuses
// the per-Link read, so a fallback to it fails the test.
type bulkMirror struct {
	fakeMirror
	calls int
	err   error
}

func (b *bulkMirror) LibrariesForLink(string) ([]store.Library, error) {
	return nil, errors.New("per-link read used although the mirror reads in bulk")
}

func (b *bulkMirror) LibrariesForLinks(ids []string) (map[string][]store.Library, error) {
	b.calls++
	if b.err != nil {
		return nil, b.err
	}
	return map[string][]store.Library{"l1": {{ID: "lib-a"}}}, nil
}

func TestLibrariesByLinkReadsOnceWhereTheMirrorCan(t *testing.T) {
	m := &bulkMirror{}
	svc := newService(t, &memStore{}, Options{Mirror: m})
	got, err := svc.LibrariesByLink([]string{"l1", "l2", "l3"})
	if err != nil {
		t.Fatal(err)
	}
	if m.calls != 1 || len(got["l1"]) != 1 || len(got["l2"]) != 0 {
		t.Errorf("calls = %d, got = %+v; want one bulk read answering l1 only", m.calls, got)
	}

	m.err = errors.New("db down")
	if _, err := svc.LibrariesByLink([]string{"l1"}); err == nil {
		t.Error("a bulk read error was swallowed")
	}
}

func TestLibrariesByLinkFallsBackToPerLinkReads(t *testing.T) {
	mm := &memMirror{libs: []store.Library{
		{ID: "lib-a", LinkID: "l1"}, {ID: "lib-b", LinkID: "l2"},
	}}
	svc := newService(t, &memStore{}, Options{Mirror: mm})
	got, err := svc.LibrariesByLink([]string{"l1", "l2", "l3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["l1"]) != 1 || len(got["l2"]) != 1 || len(got["l3"]) != 0 {
		t.Errorf("got = %+v, want l1 and l2 one each, l3 none", got)
	}
}
