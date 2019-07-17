// Copyright 2019 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/nats-io/jwt"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

const (
	audience  = "OSCON-DEMO"
	preSub    = "chat.OSCON2019."
	onlineSub = preSub + "online"
	postsSub  = preSub + "posts.*"
	postsPub  = preSub + "posts.%s"
	dmsPub    = preSub + "dms.%s"
)

// This will setup our subscriptions for the chat service.
func (s *state) setupNATS(nc *nats.Conn, creds, name string) {
	s.nc = nc
	s.me, s.skp = loadUser(creds)

	// Allow override
	if name != "" {
		s.name = displayName(name)
	} else {
		s.name = displayName(s.me.Name)
	}

	// Listen for new posts, direct msgs.
	if _, err := nc.Subscribe(postsSub, s.processNewPost); err != nil {
		log.Fatalf("Could not subscribe to new posts: %v", err)
	}

	// Only listen for DMs for us.
	dmsSub := fmt.Sprintf(dmsPub, s.me.Subject)
	if _, err := nc.Subscribe(dmsSub, s.processNewDM); err != nil {
		log.Fatalf("Could not subscribe to new DMs: %v", err)
	}

	// Watch for others coming online.
	if _, err := nc.Subscribe(onlineSub, s.processUserUpdate); err != nil {
		log.Fatalf("Could not subscribe to online status: %v", err)
	}

	// Set our status to online.
	s.sendFirstOnlineStatus()

	// Show ourselves on the DM list.
	s.addNewUser(s.name, s.me.Subject)
}

const maxNameLen = 8

func displayName(name string) string {
	fname := strings.Split(name, " ")[0]
	fname = strings.ToLower(fname)
	if len(fname) > maxNameLen {
		fname = fname[:maxNameLen]
	}
	return fname
}

const (
	onlineInterval = 1 * time.Minute
)

func (s *state) sendFirstOnlineStatus() {
	s.sendOnlineStatus(true)
}
func (s *state) sendOnlineStatusUpdate() {
	s.sendOnlineStatus(false)
}

func (s *state) sendOnlineStatus(first bool) {
	online := jwt.NewGenericClaims(s.me.Subject)
	online.Name = s.name
	online.Expires = time.Now().Add(onlineInterval).UTC().Unix() // 1 minute from now
	online.Type = jwt.ClaimType("ngs-chat-online")
	if first {
		online.Tags.Add("new")
	}
	ojwt, _ := online.Encode(s.skp)
	s.nc.Publish(onlineSub, []byte(ojwt))

	// Send periodically while running.
	time.AfterFunc(onlineInterval/2, s.sendOnlineStatusUpdate)
}

func (s *state) processUserUpdate(m *nats.Msg) {
	userClaim, err := jwt.DecodeGeneric(string(m.Data))
	if err != nil {
		log.Printf("-ERR Received a bad user update: %v", err)
		return
	}
	vr := jwt.CreateValidationResults()
	userClaim.Validate(vr)
	if vr.IsBlocking(true) {
		log.Printf("-ERR Blocking issues for user update:%+v", vr)
		return
	}

	s.Lock()
	defer s.Unlock()

	u := s.users[userClaim.Subject]
	if u == nil {
		u = s.addNewUser(userClaim.Name, userClaim.Subject)
		s.ui.Update(func() {
			s.direct.AddItems(dName(u.name))
		})
	}
	u.last = time.Now()

	if userClaim.Tags.Contains("new") {
		// Now send out status as well so they know us before next update.
		s.sendOnlineStatus(false)
	}
}

func (s *state) postSubject() string {
	var subj string
	if s.cur.kind == direct {
		if u := s.dms[s.cur.name]; u != nil {
			subj = fmt.Sprintf(dmsPub, u.nkey)
		}
	} else {
		subj = fmt.Sprintf(postsPub, s.cur.name)
	}
	return subj
}

// Called when we send a channel post
func (s *state) sendPost(m string) *postClaim {
	newPost := s.newPost(m)
	pjwt, _ := newPost.Encode(s.skp)
	s.registerPost(newPost.ID)
	s.nc.Publish(s.postSubject(), []byte(pjwt))
	return newPost
}

func checkPostClaim(claim string) *postClaim {
	post, err := jwt.DecodeGeneric(claim)
	if err != nil {
		log.Printf("-ERR Received a bad post: %v", err)
		return nil
	}
	vr := jwt.CreateValidationResults()
	post.Validate(vr)
	if vr.IsBlocking(true) {
		log.Printf("-ERR Blocking issues for post:%+v", vr)
		return nil
	}
	return &postClaim{post}
}

// Receive a new channel post from another user.
func (s *state) processNewPost(m *nats.Msg) {
	post := checkPostClaim(string(m.Data))
	if post == nil || s.posts[post.Subject] == nil {
		return
	}

	s.Lock()
	defer s.Unlock()

	if s.postIsDupe(post.ID) {
		return
	}
	s.posts[post.Subject] = append(s.posts[post.Subject], post)

	if s.cur.kind == channel && s.cur.name == post.Subject {
		s.ui.Update(func() {
			s.msgs.AppendRow(s.postEntry(post))
		})
	}
}

// Receive a new channel post from another user.
func (s *state) processNewDM(m *nats.Msg) {
	post := checkPostClaim(string(m.Data))
	if post == nil {
		return
	}

	s.Lock()
	defer s.Unlock()

	// We don't allow DMs from new users. We should know the user already.
	u := s.users[post.Issuer]
	if u == nil {
		return
	}
	u.posts = append(u.posts, post)

	// Update display if we are currently being viewed.
	if s.cur.kind == direct && s.cur.name == u.name {
		s.ui.Update(func() {
			s.msgs.AppendRow(s.postEntry(post))
		})
	}
}

var nscDecoratedRe = regexp.MustCompile(`\s*(?:(?:[-]{3,}[^\n]*[-]{3,}\n)(.+)(?:\n\s*[-]{3,}[^\n]*[-]{3,}\n))`)

func loadUser(creds string) (*jwt.UserClaims, nkeys.KeyPair) {
	contents, err := ioutil.ReadFile(creds)
	if err != nil {
		log.Fatalf("Could not load user credentials: %v", err)
	}
	items := nscDecoratedRe.FindAllSubmatch(contents, -1)
	if len(items) != 2 {
		log.Fatal("Expected user JWT and seed!")
	}
	ujwt := items[0][1]
	seed := items[1][1]

	kp, err := nkeys.FromSeed(seed)
	if err != nil {
		log.Fatalf("Could not decode seed: %v", err)
	}
	for i := range seed {
		seed[i] = 'x'
	}

	uc, err := jwt.DecodeUserClaims(string(ujwt))
	if err != nil {
		log.Fatalf("Could not decode user: %v", err)
	}
	return uc, kp
}

func setupConnOptions(opts []nats.Option) []nats.Option {
	totalWait := 10 * time.Minute
	reconnectDelay := time.Second

	opts = append(opts, nats.ReconnectWait(reconnectDelay))
	opts = append(opts, nats.MaxReconnects(int(totalWait/reconnectDelay)))
	opts = append(opts, nats.ClosedHandler(func(nc *nats.Conn) {
		log.Fatalf("Exiting: %v", nc.LastError())
	}))
	// We do not want to hear ourselves for this application.
	opts = append(opts, nats.NoEcho())

	return opts
}
