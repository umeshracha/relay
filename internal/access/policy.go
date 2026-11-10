package access

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"math"
	"net/netip"
	"strings"
	"time"
)

type Rule struct {
	ID, Scope, Target string
	Rate, Burst       int
	Observe           bool
}

type Decision struct {
	Status     int
	RuleID     string
	RetryAfter int
	Observed   string
}

type bucketKey struct{ rule, subject string }

type bucket struct {
	tokens float64
	at     time.Time
}

const maxBuckets = 65536

var errAlreadyImported = errors.New("job rule already committed")

func ValidateRule(rule *Rule) error {
	switch rule.Scope {
	case "cidr":
		prefix, err := netip.ParsePrefix(rule.Target)
		if err != nil {
			addr, err := netip.ParseAddr(rule.Target)
			if err != nil || addr.Zone() != "" {
				return ErrInput
			}
			addr = addr.Unmap()
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		rule.Target = prefix.Masked().String()
		rule.Rate, rule.Burst = 0, 0
	case "api", "team", "app", "route", "ip":
		if rule.Rate < 1 || rule.Rate > 1_000_000 || rule.Burst < 1 || rule.Burst > 1_000_000 {
			return ErrInput
		}
		if rule.Scope == "ip" {
			rule.Target = ""
		} else if rule.Target == "" || len(rule.Target) > 2048 {
			return ErrInput
		}
	default:
		return ErrInput
	}
	return nil
}

func (s *Store) AddRule(ctx context.Context, rule Rule) error {
	return s.addRule(ctx, rule, "")
}

func (s *Store) ImportRule(ctx context.Context, rule Rule, itemID string) error {
	return s.addRule(ctx, rule, itemID)
}

func (s *Store) addRule(ctx context.Context, rule Rule, itemID string) error {
	if err := ValidateRule(&rule); err != nil {
		return err
	}
	id := rand.Text()
	err := s.change(ctx, "Traffic rule added", id, func(tx *sql.Tx) error {
		if itemID != "" {
			result, err := tx.ExecContext(ctx, "INSERT INTO policy_receipts VALUES (?) ON CONFLICT DO NOTHING", itemID)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count == 0 {
				return errAlreadyImported
			}
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO gateway_rules VALUES (?,?,?,?,?,?)",
			id, rule.Scope, rule.Target, rule.Rate, rule.Burst, rule.Observe)
		return err
	})
	if errors.Is(err, errAlreadyImported) {
		return nil
	}
	return err
}

func (s *Store) RemoveRule(ctx context.Context, id string) error {
	return s.change(ctx, "Traffic rule removed", id, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "DELETE FROM gateway_rules WHERE id=?", id)
		return changed(result, err, ErrMissing)
	})
}

func (s *Store) Rules() []Rule {
	return append([]Rule(nil), s.live.Load().rules...)
}

func loadRules(ctx context.Context, db queryer) ([]Rule, error) {
	rows, err := db.QueryContext(ctx, "SELECT id,scope,target,rate,burst,observe FROM gateway_rules ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []Rule
	for rows.Next() {
		var rule Rule
		if err := rows.Scan(&rule.ID, &rule.Scope, &rule.Target, &rule.Rate, &rule.Burst, &rule.Observe); err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

func (s *Store) Evaluate(api, route string, key Key, address netip.Addr, now time.Time) Decision {
	if len(s.live.Load().rules) == 0 {
		return Decision{}
	}
	s.limitMu.Lock()
	defer s.limitMu.Unlock()
	rules := s.live.Load().rules
	var decision Decision
	for _, rule := range rules {
		if rule.Scope != "cidr" {
			continue
		}
		prefix, err := netip.ParsePrefix(rule.Target)
		if err != nil || !prefix.Contains(address) {
			continue
		}
		if !rule.Observe {
			decision.Status, decision.RuleID = 403, rule.ID
			return decision
		}
		decision.Observed = rule.ID
	}

	if s.buckets == nil {
		s.buckets = make(map[bucketKey]bucket)
	}
	if !now.Before(s.nextSweep) {
		active := make(map[string]Rule, len(rules))
		for _, rule := range rules {
			active[rule.ID] = rule
		}
		for id, state := range s.buckets {
			rule, ok := active[id.rule]
			if !ok || state.tokens+now.Sub(state.at).Seconds()*float64(rule.Rate)/60 >= float64(rule.Burst) {
				delete(s.buckets, id)
			}
		}
		s.nextSweep = now.Add(time.Minute)
	}
	var charges []bucketKey
	for _, rule := range rules {
		subject, applies := rule.subject(api, route, key, address)
		if !applies {
			continue
		}
		id := bucketKey{rule.ID, subject}
		state, exists := s.buckets[id]
		if exists {
			state.tokens = math.Min(float64(rule.Burst), state.tokens+max(0, now.Sub(state.at).Seconds())*float64(rule.Rate)/60)
		} else {
			state.tokens = float64(rule.Burst)
		}
		state.at = now
		if state.tokens < 1 || (!exists && len(s.buckets) >= maxBuckets) {
			if rule.Observe {
				decision.Observed = rule.ID
				continue
			}
			seconds := 60.0
			if exists {
				seconds = (1 - state.tokens) * 60 / float64(rule.Rate)
			}
			decision.Status, decision.RuleID, decision.RetryAfter = 429, rule.ID, max(1, int(math.Ceil(seconds)))
			break
		}
		s.buckets[id] = state
		if rule.Observe {
			state.tokens--
			s.buckets[id] = state
		} else {
			charges = append(charges, id)
		}
	}
	if decision.Status == 0 {
		for _, id := range charges {
			state := s.buckets[id]
			state.tokens--
			s.buckets[id] = state
		}
	}
	return decision
}

func (r Rule) subject(api, route string, key Key, address netip.Addr) (string, bool) {
	switch r.Scope {
	case "api":
		return "", r.Target == api
	case "team":
		return "", key.ID != "" && r.Target == key.Team
	case "app":
		return "", key.ID != "" && r.Target == key.Team+"/"+key.App
	case "route":
		return "", r.Target == strings.TrimSpace(api+" "+route)
	case "ip":
		return address.String(), key.ID == "" && address.IsValid()
	default:
		return "", false
	}
}
