// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package amass

import (
	"net"
	"strings"

	"github.com/OWASP/Amass/amass/core"
	"github.com/OWASP/Amass/amass/utils"
	"github.com/miekg/dns"
)

var (
	// InitialQueryTypes include the DNS record types that are
	// initially requested for a discovered name
	InitialQueryTypes = []string{
		"TXT",
		"CNAME",
		"A",
		"AAAA",
	}

	badSubnets = []string{
		"198.105.244.0/24",
		"198.105.254.0/24",
		"88.204.137.0/24",
	}
)

// DNSService is the Service that handles all DNS name resolution requests within
// the architecture.
type DNSService struct {
	core.BaseService

	filter        *utils.StringFilter
	cidrBlacklist []*net.IPNet
}

// NewDNSService returns he object initialized, but not yet started.
func NewDNSService(config *core.Config, bus *core.EventBus) *DNSService {
	ds := &DNSService{filter: utils.NewStringFilter()}

	for _, n := range badSubnets {
		if _, ipnet, err := net.ParseCIDR(n); err == nil {
			ds.cidrBlacklist = append(ds.cidrBlacklist, ipnet)
		}
	}

	ds.BaseService = *core.NewBaseService(ds, "DNS Service", config, bus)
	return ds
}

// OnStart implements the Service interface
func (ds *DNSService) OnStart() error {
	ds.BaseService.OnStart()

	ds.Bus().Subscribe(core.ResolveNameTopic, ds.SendRequest)
	ds.Bus().Subscribe(core.ReverseSweepTopic, ds.dnsSweep)
	ds.Bus().Subscribe(core.NewSubdomainTopic, ds.newSubdomain)
	go ds.processRequests()

	for _, domain := range ds.Config().Domains() {
		go ds.basicQueries(domain, domain)
	}
	return nil
}

func (ds *DNSService) resolvedName(req *core.Request) {
	if !TrustedTag(req.Tag) || MatchesWildcard(req) {
		return
	}
	ds.Bus().Publish(core.NameResolvedTopic, req)
}

func (ds *DNSService) processRequests() {
	for {
		select {
		case <-ds.PauseChan():
			<-ds.ResumeChan()
		case <-ds.Quit():
			return
		case req := <-ds.RequestChan():
			go ds.performRequest(req)
		}
	}
}

func (ds *DNSService) performRequest(req *core.Request) {
	if req == nil || req.Name == "" || req.Domain == "" {
		return
	}

	ds.SetActive()
	if ds.Config().Blacklisted(req.Name) || (!TrustedTag(req.Tag) &&
		GetWildcardType(req) == WildcardTypeDynamic) {
		return
	}

	var answers []core.DNSAnswer
	for _, t := range InitialQueryTypes {
		if a, err := Resolve(req.Name, t); err == nil {
			if ds.goodDNSRecords(a) {
				answers = append(answers, a...)
			}
			// Do not continue if a CNAME was discovered
			if t == "CNAME" {
				break
			}
		} else {
			ds.Config().Log.Printf("DNS: %v", err)
		}
		ds.SetActive()
	}

	req.Records = answers
	if len(req.Records) == 0 {
		// Check if this unresolved name should be output by the enumeration
		if ds.Config().IncludeUnresolvable && ds.Config().IsDomainInScope(req.Name) {
			ds.Bus().Publish(core.OutputTopic, &core.Output{
				Name:   req.Name,
				Domain: req.Domain,
				Tag:    req.Tag,
				Source: req.Source,
			})
		}
		return
	}
	ds.resolvedName(req)
}

func (ds *DNSService) goodDNSRecords(records []core.DNSAnswer) bool {
	for _, r := range records {
		if r.Type != int(dns.TypeA) {
			continue
		}

		for _, cidr := range ds.cidrBlacklist {
			if cidr.Contains(net.ParseIP(r.Data)) {
				return false
			}
		}
	}
	return true
}

func (ds *DNSService) newSubdomain(req *core.Request, times int) {
	if req != nil && times == 1 {
		go ds.processSubdomain(req)
	}
}

func (ds *DNSService) processSubdomain(req *core.Request) {
	ds.SetActive()
	ds.basicQueries(req.Name, req.Domain)
	ds.queryServiceNames(req.Name, req.Domain)
}

func (ds *DNSService) basicQueries(subdomain, domain string) {
	var answers []core.DNSAnswer

	ds.SetActive()
	// Obtain the DNS answers for the NS records related to the domain
	if ans, err := Resolve(subdomain, "NS"); err == nil {
		for _, a := range ans {
			pieces := strings.Split(a.Data, ",")
			a.Data = pieces[len(pieces)-1]

			if ds.Config().Active {
				go ds.attemptZoneXFR(subdomain, domain, a.Data)
			}
			answers = append(answers, a)
		}
	} else {
		ds.Config().Log.Printf("DNS: NS record query error: %s: %v", subdomain, err)
	}

	ds.SetActive()
	// Obtain the DNS answers for the MX records related to the domain
	if ans, err := Resolve(subdomain, "MX"); err == nil {
		for _, a := range ans {
			answers = append(answers, a)
		}
	} else {
		ds.Config().Log.Printf("DNS: MX record query error: %s: %v", subdomain, err)
	}

	ds.SetActive()
	// Obtain the DNS answers for the SOA records related to the domain
	if ans, err := Resolve(subdomain, "SOA"); err == nil {
		answers = append(answers, ans...)
	} else {
		ds.Config().Log.Printf("DNS: SOA record query error: %s: %v", subdomain, err)
	}

	ds.SetActive()
	// Obtain the DNS answers for the SPF records related to the domain
	if ans, err := Resolve(subdomain, "SPF"); err == nil {
		answers = append(answers, ans...)
	} else {
		ds.Config().Log.Printf("DNS: SPF record query error: %s: %v", subdomain, err)
	}

	if len(answers) > 0 {
		ds.SetActive()
		ds.resolvedName(&core.Request{
			Name:    subdomain,
			Domain:  domain,
			Records: answers,
			Tag:     core.DNS,
			Source:  "Forward DNS",
		})
	}
}

func (ds *DNSService) attemptZoneXFR(sub, domain, server string) {
	if ds.filter.Duplicate(sub + server) {
		return
	}

	if requests, err := ZoneTransfer(sub, domain, server); err == nil {
		for _, req := range requests {
			ds.resolvedName(req)
		}
	} else {
		ds.Config().Log.Printf("DNS: Zone XFR failed: %s: %v", sub, err)
	}
}

func (ds *DNSService) queryServiceNames(subdomain, domain string) {
	ds.SetActive()
	for _, name := range popularSRVRecords {
		srvName := name + "." + subdomain

		if ds.filter.Duplicate(srvName) {
			continue
		}
		if a, err := Resolve(srvName, "SRV"); err == nil {
			ds.resolvedName(&core.Request{
				Name:    srvName,
				Domain:  domain,
				Records: a,
				Tag:     core.DNS,
				Source:  "Forward DNS",
			})
		}
	}
}

func (ds *DNSService) dnsSweep(addr string, cidr *net.IPNet) {
	ds.SetActive()
	go ds.reverseDNSSweep(addr, cidr)
}

func (ds *DNSService) reverseDNSSweep(addr string, cidr *net.IPNet) {
	var ips []net.IP

	// Get information about nearby IP addresses
	if ds.Config().Active {
		ips = utils.CIDRSubset(cidr, addr, 500)
	} else {
		ips = utils.CIDRSubset(cidr, addr, 250)
	}

	for _, ip := range ips {
		a := ip.String()
		if ds.filter.Duplicate(a) {
			continue
		}
		ds.reverseDNSQuery(a)
	}
}

func (ds *DNSService) reverseDNSQuery(ip string) {
	ds.SetActive()
	ptr, answer, err := Reverse(ip)
	if err != nil {
		return
	}
	// Check that the name discovered is in scope
	domain := ds.Config().WhichDomain(answer)
	if domain == "" {
		return
	}
	ds.resolvedName(&core.Request{
		Name:   ptr,
		Domain: domain,
		Records: []core.DNSAnswer{{
			Name: ptr,
			Type: 12,
			TTL:  0,
			Data: answer,
		}},
		Tag:    core.DNS,
		Source: "Reverse DNS",
	})
}
