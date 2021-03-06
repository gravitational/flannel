// Copyright 2015 flannel authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// +build !windows

package network

import (
	"strings"
	"time"

	"github.com/golang/glog"

	"github.com/coreos/go-iptables/iptables"
	"github.com/gravitational/trace"
)

type IPTables interface {
	AppendUnique(table string, chain string, rulespec ...string) error
	Delete(table string, chain string, rulespec ...string) error
	Exists(table string, chain string, rulespec ...string) (bool, error)
	ClearChain(table string, chain string) error
	HasRandomFully() bool
}

type IPTablesRule struct {
	table    string
	chain    string
	rulespec []string
	comment  string
}

func (r IPTablesRule) getRule() []string {
	var comment []string
	if r.comment != "" {
		comment = []string{"-m", "comment", "--comment", r.comment}
	}
	return append(r.rulespec, comment...)
}

var (
	// flannelMasqChain is a separate 'nat' table chain to hold flannel rules for SNAT when leaving / entering
	// the overlay network.
	flannelMasqChain = chain{nat, "FLANNEL-MASQ"}

	// flannelForwardChain creates ACCEPT rules for forwarding overlay network traffic when the filter table forward
	// chain is set to DROP by default. Ensures overlay network traffic can be forwarded.
	flannelForwardChain = chain{filter, "FLANNEL-FORWARD"}

	// chains is a list of table/chains used by flannel
	chains = []chain{flannelMasqChain, flannelForwardChain}
)

type chain struct {
	table string
	name  string
}

type Config struct {
	Network string
	Lease   string
	// Setup ipMasq if configured
	Masquerade bool
	// IPTablesresyncInterval indicated how frequently to resync iptables
	IPTablesresyncInterval time.Duration
}

const (
	postrouting = "POSTROUTING"
	nat         = "nat"
	filter      = "filter"
	mangle      = "mangle"
	forward     = "FORWARD"
	input       = "INPUT"
)

// generateRules generates the iptables rule configuration required by flannel
func (c Config) generateRules(ipt IPTables, masq bool) []IPTablesRule {
	rules := make([]IPTablesRule, 0)

	if masq {
		supportsRandomFully := ipt.HasRandomFully()

		// This rule makes sure we don't NAT traffic within overlay network (e.g. coming out of docker0)
		rules = append(rules,
			IPTablesRule{flannelMasqChain.table, flannelMasqChain.name,
				[]string{"-s", c.Network, "-d", c.Network, "-j", "RETURN"},
				"flannel: internal overlay traffic"},
		)

		// NAT if it's not multicast traffic
		if supportsRandomFully {
			rules = append(rules,
				IPTablesRule{flannelMasqChain.table, flannelMasqChain.name,
					[]string{"-s", c.Network, "!", "-d", "224.0.0.0/4", "-j", "MASQUERADE", "--random-fully"},
					"flannel: nat outbound traffic"},
			)
		} else {
			rules = append(rules,
				IPTablesRule{flannelMasqChain.table, flannelMasqChain.name,
					[]string{"-s", c.Network, "!", "-d", "224.0.0.0/4", "-j", "MASQUERADE"},
					"flannel: nat outbound traffic"},
			)
		}

		// Prevent performing Masquerade on external traffic which arrives from a Node that owns the container/pod IP address
		rules = append(rules,
			IPTablesRule{flannelMasqChain.table, flannelMasqChain.name,
				[]string{"!", "-s", c.Network, "-d", c.Lease, "-j", "RETURN"},
				"flannel: preserve source ip to local"},
		)

		// Masquerade anything headed towards flannel from the host
		if supportsRandomFully {
			rules = append(rules,
				IPTablesRule{flannelMasqChain.table, flannelMasqChain.name,
					[]string{"!", "-s", c.Network, "-d", c.Network, "-j", "MASQUERADE", "--random-fully"},
					"flannel: snat to overlay"},
			)
		} else {
			rules = append(rules,
				IPTablesRule{flannelMasqChain.table, flannelMasqChain.name,
					[]string{"!", "-s", c.Network, "-d", c.Network, "-j", "MASQUERADE"},
					"flannel: snat to overlay"},
			)
		}

	}

	rules = append(rules,
		IPTablesRule{flannelForwardChain.table, flannelForwardChain.name,
			[]string{"-s", c.Network, "-j", "ACCEPT"},
			"flannel: allow forwarding of overlay traffic"},
		IPTablesRule{flannelForwardChain.table, flannelForwardChain.name,
			[]string{"-d", c.Network, "-j", "ACCEPT"},
			"flannel: allow forwarding of overlay traffic"},
	)

	rules = append(rules, c.generateJoinRules(masq)...)

	return rules
}

func (c Config) generateJoinRules(masq bool) (rules []IPTablesRule) {
	if masq {
		// Invoke the flannel masquerade chain from the nat/postrouting chain
		rules = append(rules,
			IPTablesRule{flannelMasqChain.table, postrouting,
				[]string{"-j", flannelMasqChain.name},
				"flannel: nat rules for overlay network"},
		)
	}

	rules = append(rules,
		IPTablesRule{flannelForwardChain.table, forward,
			[]string{"-j", flannelForwardChain.name},
			"flannel: allow rules for overlay network"},
	)

	return rules
}

// legacyRules returns rules that may have been generated by previous instances of flannel.
func (c Config) legacyRules(ipt IPTables) []IPTablesRule {
	rules := make([]IPTablesRule, 0)

	supportsRandomFully := ipt.HasRandomFully()

	if supportsRandomFully {
		rules = append(rules, []IPTablesRule{
			// This rule makes sure we don't NAT traffic within overlay network (e.g. coming out of docker0)
			{"nat", "POSTROUTING", []string{"-s", c.Network, "-d", c.Network, "-j", "RETURN"}, ""},
			// NAT if it's not multicast traffic
			{"nat", "POSTROUTING", []string{"-s", c.Network, "!", "-d", "224.0.0.0/4", "-j", "MASQUERADE", "--random-fully"}, ""},
			// Prevent performing Masquerade on external traffic which arrives from a Node that owns the container/pod IP address
			{"nat", "POSTROUTING", []string{"!", "-s", c.Network, "-d", c.Lease, "-j", "RETURN"}, ""},
			// Masquerade anything headed towards flannel from the host
			{"nat", "POSTROUTING", []string{"!", "-s", c.Network, "-d", c.Network, "-j", "MASQUERADE", "--random-fully"}, ""},
		}...)
	}

	rules = append(rules, []IPTablesRule{
		// This rule makes sure we don't NAT traffic within overlay network (e.g. coming out of docker0)
		{"nat", "POSTROUTING", []string{"-s", c.Network, "-d", c.Network, "-j", "RETURN"}, ""},
		// NAT if it's not multicast traffic
		{"nat", "POSTROUTING", []string{"-s", c.Network, "!", "-d", "224.0.0.0/4", "-j", "MASQUERADE"}, ""},
		// Prevent performing Masquerade on external traffic which arrives from a Node that owns the container/pod IP address
		{"nat", "POSTROUTING", []string{"!", "-s", c.Network, "-d", c.Lease, "-j", "RETURN"}, ""},
		// Masquerade anything headed towards flannel from the host
		{"nat", "POSTROUTING", []string{"!", "-s", c.Network, "-d", c.Network, "-j", "MASQUERADE"}, ""},
	}...)

	// legacy forwarding rules
	rules = append(rules, []IPTablesRule{
		// These rules allow traffic to be forwarded if it is to or from the flannel network range.
		{"filter", "FORWARD", []string{"-s", c.Network, "-j", "ACCEPT"}, ""},
		{"filter", "FORWARD", []string{"-d", c.Network, "-j", "ACCEPT"}, ""},
	}...)

	return rules
}

func (c Config) cleanupRules(ipt IPTables) {
	for _, chain := range chains {
		glog.Info("Clearing chain: table: ", chain.table, " chain: ", chain.name)

		err := ipt.ClearChain(chain.table, chain.name)
		if err != nil {
			glog.Warning("Clear chain ", chain, " failed: ", err)
		}
	}

	// remove the rules we would add to the non flannel chains
	for _, rule := range c.generateJoinRules(true) {
		glog.Info("Deleting iptables rule: table: ", rule.table, " chain: ", rule.chain, " spec: ",
			strings.Join(rule.getRule(), " "))

		// ignore errors since these rules may not exist
		_ = ipt.Delete(rule.table, rule.chain, rule.getRule()...)
	}

	for _, rule := range c.legacyRules(ipt) {
		glog.Info("Deleting legacy iptables rule: table: ", rule.table, " chain: ", rule.chain, " spec: ",
			strings.Join(rule.getRule(), " "))

		// legacy rules are likely not on the system other than rare circumstances - ignore errors
		_ = ipt.Delete(rule.table, rule.chain, rule.getRule()...)
	}
}

func (c Config) createRules(ipt IPTables) error {
	for _, chain := range chains {
		err := ipt.ClearChain(chain.table, chain.name)
		if err != nil {
			glog.Info("Clear chain ", chain, " failed: ", err)
		}
	}

	for _, rule := range c.generateRules(ipt, c.Masquerade) {
		glog.Info("Adding iptables rule: table: ", rule.table, " chain: ", rule.chain, " spec: ",
			strings.Join(rule.getRule(), " "))

		err := ipt.AppendUnique(rule.table, rule.chain, rule.getRule()...)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c Config) rulesOk(ipt IPTables) error {
	for _, rule := range c.generateRules(ipt, c.Masquerade) {
		exists, err := ipt.Exists(rule.table, rule.chain, rule.getRule()...)
		if err != nil {
			return trace.Wrap(err)
		}
		if !exists {
			return trace.NotFound("missing rule detected: %v", rule.getRule())
		}
	}
	return nil
}

func (c Config) ensureRules(ipt IPTables) {
	err := c.rulesOk(ipt)
	if err == nil {
		return
	}

	if trace.IsNotFound(err) {
		glog.Error("Discovered missing iptables rules, re-creating...")
		// rules appear to be missing
		// so we delete then recreate our rules
		c.cleanupRules(ipt)

		err = c.createRules(ipt)
		if err != nil {
			glog.Error("Error creating iptables rules: ", trace.DebugReport(err))
		}

		return
	}

	glog.Error("Error checking iptables rules: ", trace.DebugReport(err))
}

func (c Config) SetupAndEnsureIPTables() error {
	ipt, err := iptables.New()
	if err != nil {
		return trace.Wrap(err)
	}

	go func() {
		ticker := time.NewTicker(c.IPTablesresyncInterval)

		c.cleanupRules(ipt)

		err = c.createRules(ipt)
		if err != nil {
			glog.Info("Create rules failed: ", trace.DebugReport(err))
		}

		for range ticker.C {
			c.ensureRules(ipt)
		}
	}()

	return nil
}
