package main

import (
	"slices"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

func ids(p *post) []int {
	var out []int
	for _, m := range p.messages {
		out = append(out, m.ID)
	}

	return out
}

func next(t *testing.T, q *queue) *post {
	t.Helper()
	select {
	case p := <-q.posts:
		select {
		case <-p.ready:
		case <-time.After(time.Second):
			t.Fatal("post was not sealed")
		}

		return p
	case <-time.After(time.Second):
		t.Fatal("no post in queue")
	}

	return nil
}

func TestAlbumIsOnePostAndKeepsItsPlace(t *testing.T) {
	q := newQueue(50 * time.Millisecond)
	q.add(&models.Message{ID: 1})
	q.add(&models.Message{ID: 2, MediaGroupID: "a"})
	q.add(&models.Message{ID: 3, MediaGroupID: "a"})
	q.add(&models.Message{ID: 4})
	q.add(&models.Message{ID: 5, MediaGroupID: "a"})

	want := [][]int{{1}, {2, 3, 5}, {4}}
	for _, w := range want {
		if got := ids(next(t, q)); !slices.Equal(got, w) {
			t.Fatalf("got %v, want %v", got, w)
		}
	}
}

func TestCloseSealsPendingAlbum(t *testing.T) {
	q := newQueue(time.Hour)
	q.add(&models.Message{ID: 1, MediaGroupID: "a"})
	q.close()

	if got := ids(next(t, q)); len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if _, open := <-q.posts; open {
		t.Fatal("queue is not closed")
	}
}
