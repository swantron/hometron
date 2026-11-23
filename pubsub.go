package main

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"time"
)

const (
	pubsubBase = "https://pubsub.googleapis.com/v1/"
	// Google's Device Access service publishes as this group; it needs publisher on our topic.
	sdmPublisher = "group:sdm-publisher@googlegroups.com"
)

// PubSub pulls Nest events from a subscription we own.
type PubSub struct {
	g     *Google
	topic string // projects/<p>/topics/<t>
	sub   string // projects/<p>/subscriptions/<s>
}

func newPubSub(g *Google, topic, sub string) *PubSub {
	p := "projects/" + g.creds.GCPProjectID
	return &PubSub{g: g, topic: p + "/topics/" + topic, sub: p + "/subscriptions/" + sub}
}

// Ensure creates the topic, grants Google's SDM publisher on it, and creates the
// subscription. Safe to run on every start: existing resources are left alone.
func (p *PubSub) Ensure(ctx context.Context) error {
	if err := p.g.Do(ctx, "PUT", pubsubBase+p.topic, map[string]any{}, nil); err != nil && !isConflict(err) {
		return err
	}
	policy := map[string]any{"policy": map[string]any{"bindings": []map[string]any{
		{"role": "roles/pubsub.publisher", "members": []string{sdmPublisher}},
	}}}
	if err := p.g.Do(ctx, "POST", pubsubBase+p.topic+":setIamPolicy", policy, nil); err != nil {
		return err
	}
	sub := map[string]any{
		"topic":                    p.topic,
		"ackDeadlineSeconds":       30,
		"messageRetentionDuration": "3600s",          // events older than an hour aren't worth replaying
		"expirationPolicy":         map[string]any{}, // empty policy = never expire
	}
	if err := p.g.Do(ctx, "PUT", pubsubBase+p.sub, sub, nil); err != nil && !isConflict(err) {
		return err
	}
	return nil
}

func isConflict(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusConflict
}

// Run pulls messages forever, handing each decoded payload to handle, then acking.
func (p *PubSub) Run(ctx context.Context, handle func([]byte)) {
	backoff := time.Second
	for ctx.Err() == nil {
		var out struct {
			ReceivedMessages []struct {
				AckID   string `json:"ackId"`
				Message struct {
					Data string `json:"data"`
				} `json:"message"`
			} `json:"receivedMessages"`
		}
		pullCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		err := p.g.Do(pullCtx, "POST", pubsubBase+p.sub+":pull", map[string]any{"maxMessages": 50}, &out)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("pubsub pull: %v (retry in %s)", err, backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, 2*time.Minute)
			continue
		}
		backoff = time.Second
		var acks []string
		for _, m := range out.ReceivedMessages {
			if data, err := base64.StdEncoding.DecodeString(m.Message.Data); err == nil {
				handle(data)
			}
			acks = append(acks, m.AckID)
		}
		if len(acks) > 0 {
			if err := p.g.Do(ctx, "POST", pubsubBase+p.sub+":acknowledge", map[string]any{"ackIds": acks}, nil); err != nil {
				log.Printf("pubsub ack: %v", err)
			}
		}
	}
}
