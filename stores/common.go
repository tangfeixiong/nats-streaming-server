// Copyright 2016 Apcera Inc. All rights reserved.

package stores

import (
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/go-nats-streaming/pb"
	"github.com/nats-io/nats-streaming-server/logger"
	"github.com/nats-io/nats-streaming-server/spb"
	"github.com/nats-io/nats-streaming-server/util"
)

// format string used to report that limit is reached when storing
// messages.
var droppingMsgsFmt = "WARNING: Reached limits for store %q (msgs=%v/%v bytes=%v/%v), " +
	"dropping old messages to make room for new ones"

// commonStore contains everything that is common to any type of store
type commonStore struct {
	sync.RWMutex
	closed bool
	log    logger.Logger
}

// genericStore is the generic store implementation with a map of channels.
type genericStore struct {
	commonStore
	limits   *StoreLimits
	sublist  *util.Sublist
	name     string
	channels map[string]*ChannelStore
	clients  map[string]*Client
}

// genericSubStore is the generic store implementation that manages subscriptions
// for a given channel.
type genericSubStore struct {
	commonStore
	limits    SubStoreLimits
	subject   string // Can't be wildcard
	subsCount int
	maxSubID  uint64
}

// genericMsgStore is the generic store implementation that manages messages
// for a given channel.
type genericMsgStore struct {
	commonStore
	limits     MsgStoreLimits
	subject    string // Can't be wildcard
	first      uint64
	last       uint64
	lTimestamp int64 // Timestamp of last message
	totalCount int
	totalBytes uint64
	hitLimit   bool // indicates if store had to drop messages due to limit
}

////////////////////////////////////////////////////////////////////////////
// genericStore methods
////////////////////////////////////////////////////////////////////////////

// init initializes the structure of a generic store
func (gs *genericStore) init(name string, log logger.Logger, limits *StoreLimits) error {
	gs.name = name
	if limits == nil {
		limits = &DefaultStoreLimits
	}
	if err := gs.setLimits(limits); err != nil {
		return err
	}
	gs.log = log
	// Do not use limits values to create the map.
	gs.channels = make(map[string]*ChannelStore)
	gs.clients = make(map[string]*Client)
	return nil
}

// GetExclusiveLock implements the Store interface.
func (gs *genericStore) GetExclusiveLock() (bool, error) {
	// Need to be implementation specific.
	return false, ErrNotSupported
}

// Init can be used to initialize the store with server's information.
func (gs *genericStore) Init(info *spb.ServerInfo) error {
	return nil
}

// Name returns the type name of this store
func (gs *genericStore) Name() string {
	return gs.name
}

// Recover implements the Store interface.
func (gs *genericStore) Recover() (*RecoveredState, error) {
	// Implementations that can recover their state need to
	// override this.
	return nil, nil
}

// setLimits makes a copy of the given StoreLimits,
// validates the limits and if ok, applies the inheritance.
func (gs *genericStore) setLimits(limits *StoreLimits) error {
	// Make a copy.
	gs.limits = limits.Clone()
	// Build will validate and apply inheritance if no error.
	if err := gs.limits.Build(); err != nil {
		return err
	}
	// We don't need the PerChannel map and the sublist. So replace
	// the map with the sublist instead.
	gs.sublist = util.NewSublist()
	for key, val := range gs.limits.PerChannel {
		// val is already a copy of the original limits.PerChannel[key],
		// so don't need to make a copy again, we own this.
		gs.sublist.Insert(key, val)
	}
	// Get rid of the map now.
	gs.limits.PerChannel = nil
	return nil
}

// Returns the appropriate limits for this channel based on inheritance.
// The channel is assumed to be a literal, and the store lock held on entry.
func (gs *genericStore) getChannelLimits(channel string) *ChannelLimits {
	r := gs.sublist.Match(channel)
	if len(r) == 0 {
		// If there is no match, that means we need to use the global limits.
		return &gs.limits.ChannelLimits
	}
	// If there is a match, use the limits from the last element because
	// we know that the returned array is ordered from widest to narrowest,
	// and the only literal that there is would be the channel we are
	// looking up.
	return r[len(r)-1].(*ChannelLimits)
}

// SetLimits sets limits for this store
func (gs *genericStore) SetLimits(limits *StoreLimits) error {
	gs.Lock()
	err := gs.setLimits(limits)
	gs.Unlock()
	return err
}

// CreateChannel creates a ChannelStore for the given channel, and returns
// `true` to indicate that the channel is new, false if it already exists.
func (gs *genericStore) CreateChannel(channel string, userData interface{}) (*ChannelStore, bool, error) {
	// no-op
	return nil, false, fmt.Errorf("generic store: feature not implemented")
}

// LookupChannel returns a ChannelStore for the given channel.
func (gs *genericStore) LookupChannel(channel string) *ChannelStore {
	gs.RLock()
	cs := gs.channels[channel]
	gs.RUnlock()
	return cs
}

// HasChannel returns true if this store has any channel
func (gs *genericStore) HasChannel() bool {
	gs.RLock()
	l := len(gs.channels)
	gs.RUnlock()
	return l > 0
}

// GetChannelNames implements the Store interface.
func (gs *genericStore) GetChannels() map[string]*ChannelStore {
	gs.RLock()
	defer gs.RUnlock()
	res := make(map[string]*ChannelStore, len(gs.channels))
	for k, v := range gs.channels {
		copyVal := *v
		res[k] = &copyVal
	}
	return res
}

// GetChannelsCount implements the Store interface.
func (gs *genericStore) GetChannelsCount() int {
	gs.RLock()
	defer gs.RUnlock()
	return len(gs.channels)
}

// State returns message store statistics for a given channel ('*' for all)
func (gs *genericStore) MsgsState(channel string) (numMessages int, byteSize uint64, err error) {
	numMessages = 0
	byteSize = 0
	err = nil

	if channel == AllChannels {
		gs.RLock()
		cs := gs.channels
		gs.RUnlock()

		for _, c := range cs {
			n, b, lerr := c.Msgs.State()
			if lerr != nil {
				err = lerr
				return
			}
			numMessages += n
			byteSize += b
		}
	} else {
		cs := gs.LookupChannel(channel)
		if cs != nil {
			numMessages, byteSize, err = cs.Msgs.State()
		}
	}
	return
}

// canAddChannel returns true if the current number of channels is below the limit.
// Store lock is assumed to be locked.
func (gs *genericStore) canAddChannel() error {
	if gs.limits.MaxChannels > 0 && len(gs.channels) >= gs.limits.MaxChannels {
		return ErrTooManyChannels
	}
	return nil
}

// AddClient stores information about the client identified by `clientID`.
func (gs *genericStore) AddClient(clientID, hbInbox string, userData interface{}) (*Client, bool, error) {
	c := &Client{spb.ClientInfo{ID: clientID, HbInbox: hbInbox}, userData}
	gs.Lock()
	oldClient := gs.clients[clientID]
	if oldClient != nil {
		gs.Unlock()
		return oldClient, false, nil
	}
	gs.clients[c.ID] = c
	gs.Unlock()
	return c, true, nil
}

// GetClient returns the stored Client, or nil if it does not exist.
func (gs *genericStore) GetClient(clientID string) *Client {
	gs.RLock()
	c := gs.clients[clientID]
	gs.RUnlock()
	return c
}

// GetClients returns all stored Client objects, as a map keyed by client IDs.
func (gs *genericStore) GetClients() map[string]*Client {
	gs.RLock()
	clients := make(map[string]*Client, len(gs.clients))
	for k, v := range gs.clients {
		clients[k] = v
	}
	gs.RUnlock()
	return clients
}

// GetClientsCount returns the number of registered clients
func (gs *genericStore) GetClientsCount() int {
	gs.RLock()
	count := len(gs.clients)
	gs.RUnlock()
	return count
}

// DeleteClient deletes the client identified by `clientID`.
func (gs *genericStore) DeleteClient(clientID string) *Client {
	gs.Lock()
	c := gs.clients[clientID]
	if c != nil {
		delete(gs.clients, clientID)
	}
	gs.Unlock()
	return c
}

// Close closes all stores
func (gs *genericStore) Close() error {
	gs.Lock()
	defer gs.Unlock()
	if gs.closed {
		return nil
	}
	gs.closed = true
	return gs.close()
}

// close closes all stores. Store lock is assumed held on entry
func (gs *genericStore) close() error {
	var err error
	var lerr error

	for _, cs := range gs.channels {
		lerr = cs.Subs.Close()
		if lerr != nil && err == nil {
			err = lerr
		}
		lerr = cs.Msgs.Close()
		if lerr != nil && err == nil {
			err = lerr
		}
	}
	return err
}

////////////////////////////////////////////////////////////////////////////
// genericMsgStore methods
////////////////////////////////////////////////////////////////////////////

// init initializes this generic message store
func (gms *genericMsgStore) init(subject string, log logger.Logger, limits *MsgStoreLimits) {
	gms.subject = subject
	gms.limits = *limits
	gms.log = log
}

// createMsg creates a MsgProto with the given sequence number.
// A timestamp is assigned with the guarantee that it will be at least
// same than the previous message. That is, given that M1 is stored
// before M2, this ensures that:
// M1.Sequence<M2.Sequence && M1.Timestamp <= M2.Timestamp
func (gms *genericMsgStore) createMsg(seq uint64, data []byte) *pb.MsgProto {
	m := &pb.MsgProto{
		Sequence:  seq,
		Subject:   gms.subject,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	if gms.lTimestamp > 0 && m.Timestamp < gms.lTimestamp {
		m.Timestamp = gms.lTimestamp
	}
	gms.lTimestamp = m.Timestamp
	return m
}

// State returns some statistics related to this store
func (gms *genericMsgStore) State() (numMessages int, byteSize uint64, err error) {
	gms.RLock()
	c, b := gms.totalCount, gms.totalBytes
	gms.RUnlock()
	return c, b, nil
}

// Store implements the MsgStore interface
func (gms *genericMsgStore) Store(data []byte) (uint64, error) {
	// no-op
	return 0, nil
}

// FirstSequence returns sequence for first message stored.
func (gms *genericMsgStore) FirstSequence() (uint64, error) {
	gms.RLock()
	first := gms.first
	gms.RUnlock()
	return first, nil
}

// LastSequence returns sequence for last message stored.
func (gms *genericMsgStore) LastSequence() (uint64, error) {
	gms.RLock()
	last := gms.last
	gms.RUnlock()
	return last, nil
}

// FirstAndLastSequence returns sequences for the first and last messages stored.
func (gms *genericMsgStore) FirstAndLastSequence() (uint64, uint64, error) {
	gms.RLock()
	first, last := gms.first, gms.last
	gms.RUnlock()
	return first, last, nil
}

// Lookup returns the stored message with given sequence number.
func (gms *genericMsgStore) Lookup(seq uint64) (*pb.MsgProto, error) {
	// no-op
	return nil, nil
}

// FirstMsg returns the first message stored.
func (gms *genericMsgStore) FirstMsg() (*pb.MsgProto, error) {
	// no-op
	return nil, nil
}

// LastMsg returns the last message stored.
func (gms *genericMsgStore) LastMsg() (*pb.MsgProto, error) {
	// no-op
	return nil, nil
}

func (gms *genericMsgStore) Flush() error {
	// no-op
	return nil
}

// GetSequenceFromTimestamp returns the sequence of the first message whose
// timestamp is greater or equal to given timestamp.
func (gms *genericMsgStore) GetSequenceFromTimestamp(timestamp int64) (uint64, error) {
	// no-op
	return 0, nil
}

// Close closes this store.
func (gms *genericMsgStore) Close() error {
	return nil
}

////////////////////////////////////////////////////////////////////////////
// genericSubStore methods
////////////////////////////////////////////////////////////////////////////

// init initializes the structure of a generic sub store
func (gss *genericSubStore) init(channel string, log logger.Logger, limits *SubStoreLimits) {
	gss.subject = channel
	gss.limits = *limits
	gss.log = log
}

// CreateSub records a new subscription represented by SubState. On success,
// it records the subscription's ID in SubState.ID. This ID is to be used
// by the other SubStore methods.
func (gss *genericSubStore) CreateSub(sub *spb.SubState) error {
	gss.Lock()
	err := gss.createSub(sub)
	gss.Unlock()
	return err
}

// UpdateSub updates a given subscription represented by SubState.
func (gss *genericSubStore) UpdateSub(sub *spb.SubState) error {
	return nil
}

// createSub is the unlocked version of CreateSub that can be used by
// non-generic implementations.
func (gss *genericSubStore) createSub(sub *spb.SubState) error {
	if gss.limits.MaxSubscriptions > 0 && gss.subsCount >= gss.limits.MaxSubscriptions {
		return ErrTooManySubs
	}

	// Bump the max value before assigning it to the new subscription.
	gss.maxSubID++
	gss.subsCount++

	// This new subscription has the max value.
	sub.ID = gss.maxSubID

	return nil
}

// DeleteSub invalidates this subscription.
func (gss *genericSubStore) DeleteSub(subid uint64) error {
	gss.Lock()
	gss.subsCount--
	gss.Unlock()
	return nil
}

// AddSeqPending adds the given message seqno to the given subscription.
func (gss *genericSubStore) AddSeqPending(subid, seqno uint64) error {
	// no-op
	return nil
}

// AckSeqPending records that the given message seqno has been acknowledged
// by the given subscription.
func (gss *genericSubStore) AckSeqPending(subid, seqno uint64) error {
	// no-op
	return nil
}

// Flush is for stores that may buffer operations and need them to be persisted.
func (gss *genericSubStore) Flush() error {
	// no-op
	return nil
}

// Close closes this store
func (gss *genericSubStore) Close() error {
	// no-op
	return nil
}
