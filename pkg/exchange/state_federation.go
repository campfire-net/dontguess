package exchange

import "time"

// trackSenderHopDepth records the provenance hop depth for a message sender.
// Hop depth is approximated from len(msg.Antecedents): a message with no
// antecedents was sent directly (hop depth 0); each additional relay hop adds
// one antecedent. Caller must hold s.mu.
//
// F4 advisory: Antecedents length is sender-controlled and not cryptographically
// verified by the campfire protocol. An attacker can set Antecedents=[] to minimize
// hop depth and maximize the depth term in the trust formula. This is a known
// design limitation (see §4A F4 permanent constraint): hop depth is weighted at
// 0.15 and serves as a corroborating signal only. The primary trust signal is
// behavioral history (0.70 weight). Do not rely on hop depth alone for access control.
//
// Per-message-ID dedup guard (dontguess-f86, hopDepthCounted): this is called
// unconditionally from applyLocked for EVERY message, with no existing
// per-message idempotency check. A concurrent rebuildAndDispatchGapLocal
// state.Replay racing foldAndDispatchLocalSnapshot's unlocked incremental
// Apply loop can fold the same message twice (see State.foldDenialCounted
// doc for the exact interleave), appending a duplicate hop-depth sample and
// skewing the median-based FederationNodeProfile.TrustScore hop-depth term.
func (s *State) trackSenderHopDepth(msg *Message) {
	if _, dup := s.hopDepthCounted[msg.ID]; dup {
		return
	}
	s.hopDepthCounted[msg.ID] = struct{}{}
	hopDepth := len(msg.Antecedents)
	key := msg.Sender
	prof, ok := s.federationProfiles[key]
	if !ok {
		prof = &FederationNodeProfile{
			SenderKey:   key,
			TrustScore:  NewNodeTrustScoreStart,
			FirstSeenAt: time.Unix(0, msg.Timestamp),
		}
		s.federationProfiles[key] = prof
	}
	s.senderHopDepth[key] = append(s.senderHopDepth[key], hopDepth)
	// Cap the window to avoid unbounded growth.
	if len(s.senderHopDepth[key]) > SenderHopDepthWindowSize {
		s.senderHopDepth[key] = s.senderHopDepth[key][len(s.senderHopDepth[key])-SenderHopDepthWindowSize:]
	}
	prof.HopDepth = medianInt(s.senderHopDepth[key])
}

// UpdateFederationProfile updates the trust_score for a sender key based on
// observed hop depth history and transaction count. Called by the slow loop
// after computing the trust_score from behavioral signals.
//
// senderKey must be non-empty. hopDepth is the latest observed hop depth for
// this sender (appended to the existing history).
//
// Thread-safe.
func (s *State) UpdateFederationProfile(senderKey string, hopDepth int) {
	if senderKey == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.senderHopDepth[senderKey] = append(s.senderHopDepth[senderKey], hopDepth)
	// Cap the window to avoid unbounded growth (UpdateFederationProfile).
	if len(s.senderHopDepth[senderKey]) > SenderHopDepthWindowSize {
		s.senderHopDepth[senderKey] = s.senderHopDepth[senderKey][len(s.senderHopDepth[senderKey])-SenderHopDepthWindowSize:]
	}
	prof, ok := s.federationProfiles[senderKey]
	if !ok {
		prof = &FederationNodeProfile{
			SenderKey:   senderKey,
			TrustScore:  NewNodeTrustScoreStart,
			FirstSeenAt: time.Now(),
		}
		s.federationProfiles[senderKey] = prof
	}
	prof.HopDepth = medianInt(s.senderHopDepth[senderKey])
}

// SetFederationTrustScore writes the slow-loop-computed trust_score for a sender.
// Thread-safe.
func (s *State) SetFederationTrustScore(senderKey string, score float64) {
	if senderKey == "" {
		return
	}
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prof, ok := s.federationProfiles[senderKey]
	if !ok {
		prof = &FederationNodeProfile{
			SenderKey:   senderKey,
			TrustScore:  NewNodeTrustScoreStart,
			FirstSeenAt: time.Now(),
		}
		s.federationProfiles[senderKey] = prof
	}
	prof.TrustScore = score
}

// FederationProfile returns the trust profile for a sender key, or nil if
// the sender has not been observed. Thread-safe.
func (s *State) FederationProfile(senderKey string) *FederationNodeProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.federationProfiles[senderKey]
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// SenderHopDepths returns a copy of the hop depth history for a sender key.
// Used by the slow loop to compute the median hop depth for trust_score.
// Thread-safe.
func (s *State) SenderHopDepths(senderKey string) []int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.senderHopDepth[senderKey]
	if len(src) == 0 {
		return nil
	}
	out := make([]int, len(src))
	copy(out, src)
	return out
}

// AllFederationProfileKeys returns all sender keys with a federation profile.
// Used by the slow loop to iterate over all tracked senders.
// Thread-safe.
func (s *State) AllFederationProfileKeys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.federationProfiles))
	for k := range s.federationProfiles {
		keys = append(keys, k)
	}
	return keys
}

// IncrementFederationTransactionCount increments the completed transaction count
// for a sender. Called from applySettle when a settle:complete is processed.
// Caller must hold s.mu.
func (s *State) incrementFederationTransactionCount(senderKey string) {
	if senderKey == "" {
		return
	}
	prof, ok := s.federationProfiles[senderKey]
	if !ok {
		return
	}
	prof.TransactionCount++
}

// medianInt returns the median of a slice of ints. Returns 0 for an empty slice.
// Does NOT modify the input slice (works on a copy).
func medianInt(vals []int) int {
	if len(vals) == 0 {
		return 0
	}
	cp := make([]int, len(vals))
	copy(cp, vals)
	// Insertion sort — hop depth slices are small in practice.
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j] < cp[j-1]; j-- {
			cp[j], cp[j-1] = cp[j-1], cp[j]
		}
	}
	mid := len(cp) / 2
	if len(cp)%2 == 0 {
		return (cp[mid-1] + cp[mid]) / 2
	}
	return cp[mid]
}
