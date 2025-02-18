/*
 * Copyright 2019 SAP SE or an SAP affiliate company. All rights reserved. h file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 *
 */

package provider

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gardener/controller-manager-library/pkg/logger"
	"github.com/gardener/external-dns-management/pkg/server/metrics"

	"github.com/gardener/external-dns-management/pkg/dns"
	"github.com/gardener/external-dns-management/pkg/dns/provider/errors"
)

type StateTTLGetter func(zoneid dns.ZoneID) time.Duration

type ZoneCacheFactory struct {
	context               context.Context
	logger                logger.LogContext
	zonesTTL              time.Duration
	zoneStates            *zoneStates
	disableZoneStateCache bool
}

func (c ZoneCacheFactory) CreateZoneCache(cacheType ZoneCacheType, metrics Metrics, zonesUpdater ZoneCacheZoneUpdater, stateUpdater ZoneCacheStateUpdater) (ZoneCache, error) {
	common := abstractZonesCache{zonesTTL: c.zonesTTL, logger: c.logger, zonesUpdater: zonesUpdater, stateUpdater: stateUpdater}
	switch cacheType {
	case CacheZonesOnly:
		cache := &onlyZonesCache{abstractZonesCache: common}
		return cache, nil
	case CacheZoneState:
		if c.disableZoneStateCache {
			cache := &onlyZonesCache{abstractZonesCache: common}
			return cache, nil
		}
		return newDefaultZoneCache(c.zoneStates, common, metrics)
	default:
		return nil, fmt.Errorf("unknown zone cache type: %v", cacheType)
	}
}

// ZoneCacheType is the zone cache type.
type ZoneCacheType int

const (
	// CacheZonesOnly only caches the zones of the account, but not the zone state itself.
	CacheZonesOnly ZoneCacheType = iota
	// CacheZoneState caches both zones of the account and the zone states as needed.
	CacheZoneState
)

func NewTestZoneCacheFactory(zonesTTL, stateTTL time.Duration) *ZoneCacheFactory {
	return &ZoneCacheFactory{
		zonesTTL:   zonesTTL,
		zoneStates: newZoneStates(func(id dns.ZoneID) time.Duration { return stateTTL }),
	}
}

type ZoneCacheZoneUpdater func(cache ZoneCache) (DNSHostedZones, error)

type ZoneCacheStateUpdater func(zone DNSHostedZone, cache ZoneCache) (DNSZoneState, error)

type ZoneCache interface {
	GetZones() (DNSHostedZones, error)
	GetZoneState(zone DNSHostedZone) (DNSZoneState, error)
	ApplyRequests(logctx logger.LogContext, err error, zone DNSHostedZone, reqs []*ChangeRequest)
	ForwardedDomainsCache() ForwardedDomainsCache
	Release()
	ReportZoneStateConflict(zone DNSHostedZone, err error) bool
}

type ForwardedDomainsCache interface {
	Get(zoneid dns.ZoneID) []string
	Set(zoneid dns.ZoneID, value []string)
}

type forwardedDomainsCacheImpl struct {
	lock             sync.Mutex
	forwardedDomains map[dns.ZoneID][]string
}

func newForwardedDomainsCacheImpl() *forwardedDomainsCacheImpl {
	return &forwardedDomainsCacheImpl{forwardedDomains: map[dns.ZoneID][]string{}}
}

func (hd *forwardedDomainsCacheImpl) Get(zoneid dns.ZoneID) []string {
	hd.lock.Lock()
	defer hd.lock.Unlock()
	return hd.forwardedDomains[zoneid]
}

func (hd *forwardedDomainsCacheImpl) Set(zoneid dns.ZoneID, value []string) {
	hd.lock.Lock()
	defer hd.lock.Unlock()

	if value != nil {
		hd.forwardedDomains[zoneid] = value
	} else {
		delete(hd.forwardedDomains, zoneid)
	}
}

func (hd *forwardedDomainsCacheImpl) DeleteZone(zoneID dns.ZoneID) {
	hd.lock.Lock()
	defer hd.lock.Unlock()

	delete(hd.forwardedDomains, zoneID)
}

type abstractZonesCache struct {
	logger       logger.LogContext
	zonesTTL     time.Duration
	zones        DNSHostedZones
	zonesErr     error
	zonesNext    time.Time
	zonesUpdater ZoneCacheZoneUpdater
	stateUpdater ZoneCacheStateUpdater
}

type onlyZonesCache struct {
	abstractZonesCache
	lock                  sync.Mutex
	forwardedDomainsCache ForwardedDomainsCache
}

var _ ZoneCache = &onlyZonesCache{}

func (c *onlyZonesCache) GetZones() (DNSHostedZones, error) {
	zones, err := c.zonesUpdater(c)
	return zones, err
}

func (c *onlyZonesCache) GetZoneState(zone DNSHostedZone) (DNSZoneState, error) {
	state, err := c.stateUpdater(zone, c)
	return state, err
}

func (c *onlyZonesCache) ApplyRequests(logctx logger.LogContext, err error, zone DNSHostedZone, reqs []*ChangeRequest) {
}

func (c *onlyZonesCache) ForwardedDomainsCache() ForwardedDomainsCache {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.forwardedDomainsCache == nil {
		c.forwardedDomainsCache = newForwardedDomainsCacheImpl()
	}
	return c.forwardedDomainsCache
}

func (c *onlyZonesCache) ReportZoneStateConflict(zone DNSHostedZone, err error) bool {
	return false
}

func (c *onlyZonesCache) Release() {
}

type defaultZoneCache struct {
	abstractZonesCache
	lock       sync.Mutex
	logger     logger.LogContext
	metrics    Metrics
	zoneStates *zoneStates

	backoffOnError time.Duration
}

var _ ZoneCache = &defaultZoneCache{}

func newDefaultZoneCache(zoneStates *zoneStates, common abstractZonesCache, metrics Metrics) (*defaultZoneCache, error) {
	cache := &defaultZoneCache{abstractZonesCache: common, logger: common.logger, metrics: metrics, zoneStates: zoneStates}
	return cache, nil
}

func (c *defaultZoneCache) GetZones() (DNSHostedZones, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if time.Now().After(c.zonesNext) {
		c.zones, c.zonesErr = c.zonesUpdater(c)
		updateTime := time.Now()
		if c.zonesErr != nil {
			// if getzones fails, don't wait zonesTTL, but use an exponential backoff
			// to recover fast from temporary failures like throttling, network problems...
			backoff := c.nextBackoff()
			c.zonesNext = updateTime.Add(backoff)
		} else {
			c.clearBackoff()
			c.zonesNext = updateTime.Add(c.zonesTTL)
		}
		c.zoneStates.UpdateUsedZones(c, toSortedZoneIDs(c.zones))
	} else {
		c.metrics.AddGenericRequests(M_CACHED_GETZONES, 1)
	}
	return c.zones, c.zonesErr
}

func (c *defaultZoneCache) nextBackoff() time.Duration {
	next := c.backoffOnError*5/4 + 2*time.Second
	maxBackoff := c.zonesTTL / 4
	if next > maxBackoff {
		next = maxBackoff
	}
	c.backoffOnError = next
	return next
}

func (c *defaultZoneCache) clearBackoff() {
	c.backoffOnError = 0
}

func (c *defaultZoneCache) GetZoneState(zone DNSHostedZone) (DNSZoneState, error) {
	state, cached, err := c.zoneStates.GetZoneState(zone, c)
	if cached {
		c.metrics.AddZoneRequests(zone.Id().ID, M_CACHED_GETZONESTATE, 1)
	}
	return state, err
}

func (c *defaultZoneCache) ReportZoneStateConflict(zone DNSHostedZone, err error) bool {
	return c.zoneStates.ReportZoneStateConflict(zone.Id(), err)
}

func (c *defaultZoneCache) cleanZoneState(zoneID dns.ZoneID) {
	c.zoneStates.CleanZoneState(zoneID)
}

func (c *defaultZoneCache) ApplyRequests(logctx logger.LogContext, err error, zone DNSHostedZone, reqs []*ChangeRequest) {
	if err == nil {
		c.zoneStates.ExecuteRequests(zone.Id(), reqs)
	} else {
		if !errors.IsThrottlingError(err) {
			logctx.Infof("zone cache discarded because of error during ExecuteRequests")
			c.cleanZoneState(zone.Id())
			metrics.AddZoneCacheDiscarding(zone.Id())
		} else {
			logctx.Infof("zone cache untouched (only throttling during ExecuteRequests)")
		}
	}
}

func (c *defaultZoneCache) ForwardedDomainsCache() ForwardedDomainsCache {
	return c.zoneStates.ForwardedDomainsCache()
}

func (c *defaultZoneCache) Release() {
	c.zoneStates.UpdateUsedZones(c, nil)
}

type zoneStateProxy struct {
	lock            sync.Mutex
	lastUpdateStart time.Time
	lastUpdateEnd   time.Time
}

type zoneStates struct {
	lock                  sync.Mutex
	stateTTLGetter        StateTTLGetter
	inMemory              *InMemory
	proxies               map[dns.ZoneID]*zoneStateProxy
	usedZones             map[ZoneCache][]dns.ZoneID
	forwardedDomainsCache *forwardedDomainsCacheImpl
}

func newZoneStates(stateTTLGetter StateTTLGetter) *zoneStates {
	return &zoneStates{
		inMemory:              NewInMemory(),
		stateTTLGetter:        stateTTLGetter,
		proxies:               map[dns.ZoneID]*zoneStateProxy{},
		usedZones:             map[ZoneCache][]dns.ZoneID{},
		forwardedDomainsCache: newForwardedDomainsCacheImpl(),
	}
}

func (s *zoneStates) getProxy(zoneID dns.ZoneID) *zoneStateProxy {
	s.lock.Lock()
	defer s.lock.Unlock()
	proxy := s.proxies[zoneID]
	if proxy == nil {
		proxy = &zoneStateProxy{}
		s.proxies[zoneID] = proxy
	}
	return proxy
}

func (s *zoneStates) GetZoneState(zone DNSHostedZone, cache *defaultZoneCache) (DNSZoneState, bool, error) {
	proxy := s.getProxy(zone.Id())
	proxy.lock.Lock()
	defer proxy.lock.Unlock()

	start := time.Now()
	ttl := s.stateTTLGetter(zone.Id())
	if start.After(proxy.lastUpdateEnd.Add(ttl)) {
		state, err := cache.stateUpdater(zone, cache)
		if err == nil {
			proxy.lastUpdateStart = start
			proxy.lastUpdateEnd = time.Now()
			s.inMemory.SetZone(zone, state)
		} else {
			s.cleanZoneState(zone.Id(), proxy)
		}
		return state, false, err
	}

	state, err := s.inMemory.CloneZoneState(zone)
	if err != nil {
		return nil, true, err
	}
	return state, true, nil
}

func (s *zoneStates) ReportZoneStateConflict(zoneID dns.ZoneID, err error) bool {
	proxy := s.getProxy(zoneID)
	proxy.lock.Lock()
	defer proxy.lock.Unlock()

	if !proxy.lastUpdateStart.IsZero() {
		ownerConflict, ok := err.(*errors.AlreadyBusyForOwner)
		if ok {
			if ownerConflict.EntryCreatedAt.After(proxy.lastUpdateStart) {
				// If a DNSEntry ownership is moved to another DNS controller manager (e.g. shoot recreation on another seed)
				// the zone cache may have stale owner information. In this case the cache is invalidated
				// if the entry is newer than the last cache refresh.
				s.cleanZoneState(zoneID, proxy)
				return true
			}
		}
	}
	return false
}

func (s *zoneStates) ExecuteRequests(zoneID dns.ZoneID, reqs []*ChangeRequest) {
	proxy := s.getProxy(zoneID)
	proxy.lock.Lock()
	defer proxy.lock.Unlock()

	var err error
	nullMetrics := &NullMetrics{}
	for _, req := range reqs {
		err = s.inMemory.Apply(zoneID, req, nullMetrics)
		if err != nil {
			break
		}
	}

	if err != nil {
		s.cleanZoneState(zoneID, proxy)
	}
}

func (s *zoneStates) ForwardedDomainsCache() ForwardedDomainsCache {
	return s.forwardedDomainsCache
}

func (s *zoneStates) CleanZoneState(zoneID dns.ZoneID) {
	control := s.getProxy(zoneID)
	control.lock.Lock()
	defer control.lock.Unlock()

	s.cleanZoneState(zoneID, control)
}

func (s *zoneStates) cleanZoneState(zoneID dns.ZoneID, proxy *zoneStateProxy) {
	s.inMemory.DeleteZone(zoneID)
	if s.forwardedDomainsCache != nil {
		s.forwardedDomainsCache.DeleteZone(zoneID)
	}
	if proxy != nil {
		var zero time.Time
		proxy.lastUpdateStart = zero
		proxy.lastUpdateEnd = zero
	}
}

func (s *zoneStates) UpdateUsedZones(cache ZoneCache, zoneids []dns.ZoneID) {
	s.lock.Lock()
	defer s.lock.Unlock()

	oldids := s.usedZones[cache]
	if len(zoneids) == 0 {
		if len(oldids) == 0 {
			return
		}
		delete(s.usedZones, cache)
	} else {
		if reflect.DeepEqual(oldids, zoneids) {
			return
		}
		s.usedZones[cache] = zoneids
	}

	allUsed := map[dns.ZoneID]struct{}{}
	for _, zoneids := range s.usedZones {
		for _, id := range zoneids {
			allUsed[id] = struct{}{}
		}
	}

	for _, zone := range s.inMemory.GetZones() {
		if _, ok := allUsed[zone.Id()]; !ok {
			s.cleanZoneState(zone.Id(), nil)
		}
	}
	for id := range s.proxies {
		if _, ok := allUsed[id]; !ok {
			delete(s.proxies, id)
		}
	}
}

func toSortedZoneIDs(zones DNSHostedZones) []dns.ZoneID {
	if len(zones) == 0 {
		return nil
	}
	zoneids := make([]dns.ZoneID, len(zones))
	for i, zone := range zones {
		zoneids[i] = zone.Id()
	}
	sort.Slice(zoneids, func(i, j int) bool {
		cmp1 := strings.Compare(zoneids[i].ProviderType, zoneids[j].ProviderType)
		cmp2 := strings.Compare(zoneids[i].ID, zoneids[j].ID)
		return cmp1 < 0 || cmp1 == 0 && cmp2 < 0
	})
	return zoneids
}
