package main

import (
	"sync"
	"time"

	"github.com/go-telegram/bot/models"
)

// post is one channel post: a single message or an album.
type post struct {
	messages []*models.Message
	ready    chan struct{} // closed once the post is complete
}

var readyNow = func() chan struct{} {
	c := make(chan struct{})
	close(c)

	return c
}()

// queue hands out posts strictly in publication order. Telegram delivers an
// album as separate messages, so the album takes its place in the queue at
// once and counts as complete when no new parts arrive for albumWait.
//
// add is called from a single goroutine (the update handler runs
// synchronously); otherwise post order is not guaranteed.
type queue struct {
	posts     chan *post
	albumWait time.Duration

	mu     sync.Mutex
	albums map[string]*album
}

type album struct {
	post  *post
	timer *time.Timer
}

func newQueue(albumWait time.Duration) *queue {
	return &queue{
		posts:     make(chan *post, 100),
		albumWait: albumWait,
		albums:    map[string]*album{},
	}
}

func (q *queue) add(msg *models.Message) {
	group := msg.MediaGroupID
	if group == "" {
		q.posts <- &post{messages: []*models.Message{msg}, ready: readyNow}

		return
	}

	q.mu.Lock()
	if a, ok := q.albums[group]; ok {
		a.post.messages = append(a.post.messages, msg)
		a.timer.Reset(q.albumWait)
		q.mu.Unlock()

		return
	}
	p := &post{messages: []*models.Message{msg}, ready: make(chan struct{})}
	q.albums[group] = &album{post: p, timer: time.AfterFunc(q.albumWait, func() { q.seal(group, p) })}
	q.mu.Unlock()

	// Sent outside the lock: with a full queue, the timer of the album the
	// worker is waiting for could not seal it otherwise
	q.posts <- p
}

func (q *queue) seal(group string, p *post) {
	q.mu.Lock()
	a, ok := q.albums[group]
	if !ok || a.post != p {
		q.mu.Unlock()

		return
	}
	delete(q.albums, group)
	q.mu.Unlock()
	close(p.ready)
}

// close seals unfinished albums and closes the queue. Call it once no more
// updates will arrive.
func (q *queue) close() {
	q.mu.Lock()
	for group, a := range q.albums {
		a.timer.Stop()
		delete(q.albums, group)
		close(a.post.ready)
	}
	q.mu.Unlock()
	close(q.posts)
}
