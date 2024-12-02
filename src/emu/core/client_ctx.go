// Copyright (c) 2020 Cisco Systems and/or its affiliates.
// Licensed under the Apache License, Version 2.0 (the "License");
// that can be found in the LICENSE file in the root of the source
// tree.
package core

import (
	"bytes"
	"encoding/binary"
	"external/google/gopacket/layers"
	"fmt"
	"net"
	"time"
	"unsafe"
)

type CClientStats struct {
	addClients    uint64
	removeClients uint64
	activeClients uint64
}

type PbitList [5]uint8

func NewClientStatsCounterDb(o *CClientStats) *CCounterDb {
	db := NewCCounterDb("client")
	db.Add(&CCounterRec{
		Counter:  &o.addClients,
		Name:     "addClients",
		Help:     "clients add",
		Unit:     "ops",
		DumpZero: false,
		Info:     ScINFO})
	db.Add(&CCounterRec{
		Counter:  &o.removeClients,
		Name:     "removeClients",
		Help:     "clients remove",
		Unit:     "ops",
		DumpZero: false,
		Info:     ScINFO})
	db.Add(&CCounterRec{
		Counter:  &o.activeClients,
		Name:     "active clients",
		Help:     "active clients",
		Unit:     "",
		DumpZero: false,
		Info:     ScINFO})
	return db
}

func castDlistClient(dlist *DList) *CClient {
	return (*CClient)(unsafe.Pointer(uintptr(unsafe.Pointer(dlist))))
}

// CClientDg default GW
type CClientDg struct {
	IpdgResolved bool   `json:"resolve"` // bool in case it is resolved
	IpdgMac      MACKey `json:"rmac"`    // default
}

// CClientIpv6Nd information from learned from router
type CClientIpv6Nd struct {
	MTU        uint16  `json:"mtu"`   // MTU in L3 1500 by default
	DgMac      MACKey  `json:"dgmac"` // router dg
	PrefixIpv6 Ipv6Key `json:"prefix"`
	PrefixLen  uint8   `json:"prefix_len"`
	IPv6       Ipv6Key `json:"ipv6"`
}

// CClient represent one client
type CClient struct {
	dlist  DList   // for adding into list
	Ns     *CNSCtx // pointer to a namespace
	Ipv4   Ipv4Key // source ipv4
	Maskv4 Ipv4Key // mask default 0xffffffff
	DgIpv4 Ipv4Key // default gateway for ipv4
	Mac    MACKey  // immutable over lifetime of client
	MTU    uint16  // MTU in L3 1500 by default

	DGW *CClientDg /* resolve by ARP */

	Ipv6Router *CClientIpv6Nd
	Ipv6DGW    *CClientDg /* resolve by ipv6 */
	Ipv6       Ipv6Key    // set the self ipv6 by user
	DgIpv6     Ipv6Key    // default gateway if provided would be in highest priority
	Dhcpv6     Ipv6Key    // the dhcpv6 ipv6, another ipv6 would be the one that was learned from the router

	Ipv6ForceDGW   bool /* true in case we want to enforce default gateway MAC */
	Ipv6ForcedgMac MACKey

	ForceDGW       bool /* true in case we want to enforce default gateway MAC */
	Ipv4ForcedgMac MACKey

	PluginCtx *PluginCtx

	transport interface{} // pointer to transport, allocated only if needed

	timerw             *TimerCtx  // Timer Wheel
	timer              CHTimerObj // Timer Object for notifying in case DG MAC resolved.
	bitMask            uint8      // Bit mask for resolving message
	resolveAttempts    uint8      // Counter counting how many times have we tried to resolve
	maxResolveAttempts uint8      // Maximum amount of resolves allowed

	PbitList PbitList //pbit list
}

type CClientCmd struct {
	Mac    MACKey  `json:"mac" validate:"required"`
	Ipv4   Ipv4Key `json:"ipv4"`
	DgIpv4 Ipv4Key `json:"ipv4_dg"`
	MTU    uint16  `json:"ipv4_mtu"`

	Ipv6   Ipv6Key `json:"ipv6"`
	DgIpv6 Ipv6Key `json:"dg_ipv6"`

	Ipv6ForceDGW   bool   `json:"ipv6_force_dg"`
	Ipv6ForcedgMac MACKey `json:"ipv6_force_mac"`
	ForceDGW       bool   `json:"ipv4_force_dg"`
	Ipv4ForcedgMac MACKey `json:"ipv4_force_mac"`

	Plugins *MapJsonPlugs `json:"plugs"`

	PbitList PbitList `json:"pbit_list"`
}

type CClientCmds struct {
	Clients []CClientCmd `json:"clients" validate:"required"`
}

type CClientInfo struct {
	Mac    MACKey  `json:"mac"`
	Ipv4   Ipv4Key `json:"ipv4"`
	DgIpv4 Ipv4Key `json:"ipv4_dg"`
	MTU    uint16  `json:"ipv4_mtu"`

	Ipv6Local Ipv6Key `json:"ipv6_local"`
	Ipv6Slaac Ipv6Key `json:"ipv6_slaac"`
	Ipv6      Ipv6Key `json:"ipv6"`
	DgIpv6    Ipv6Key `json:"dg_ipv6"`
	DhcpIpv6  Ipv6Key `json:"dhcp_ipv6"`

	Ipv6ForceDGW   bool   `json:"ipv6_force_dg"`
	Ipv6ForcedgMac MACKey `json:"ipv6_force_mac"`
	ForceDGW       bool   `json:"ipv4_force_dg"`
	Ipv4ForcedgMac MACKey `json:"ipv4_force_mac"`

	DGW *CClientDg `json:"dgw"`

	Ipv6Router *CClientIpv6Nd `json:"ipv6_router"`
	Ipv6DGW    *CClientDg     `json:"ipv6_dgw"`

	PlugNames []string `json:"plug_names"`

	PbitList PbitList `json:"pbit_list"`
}

/* NewClient Create a new client with default information and key */
func NewClient(ns *CNSCtx,
	Mac MACKey,
	Ipv4 Ipv4Key,
	Ipv6 Ipv6Key,
	DgIpv4 Ipv4Key,
) *CClient {
	o := new(CClient)
	o.DGW = nil
	o.ForceDGW = false
	o.Ns = ns
	o.Mac = Mac
	o.Ipv4 = Ipv4
	o.Ipv6 = Ipv6
	o.DgIpv4 = DgIpv4
	o.Maskv4 = [4]byte{0xff, 0xff, 0xff, 0xff}
	o.MTU = 1500
	o.PluginCtx = NewPluginCtx(o, ns, ns.ThreadCtx, PLUGIN_LEVEL_CLIENT)
	return o
}

func NewClientCmd(ns *CNSCtx, cmd *CClientCmd) *CClient {

	c := NewClient(ns, cmd.Mac, cmd.Ipv4, cmd.Ipv6, cmd.DgIpv4)
	if cmd.MTU > 0 {
		c.MTU = cmd.MTU
	}

	c.DgIpv6 = cmd.DgIpv6

	c.Ipv6ForceDGW = cmd.Ipv6ForceDGW
	c.Ipv6ForcedgMac = cmd.Ipv6ForcedgMac
	c.ForceDGW = cmd.ForceDGW
	c.Ipv4ForcedgMac = cmd.Ipv4ForcedgMac
	c.PbitList = cmd.PbitList
	return c
}

// AttemptResolve tells the client to start attempting to resolve.
func (o *CClient) AttemptResolve() {
	o.maxResolveAttempts = 5                // Maximum amount of resolved attempts.
	o.timerw = o.Ns.ThreadCtx.GetTimerCtx() // Initialize timer wheel
	o.timer.SetCB(o, 0, 0)                  // Set callback for timer
	o.OnEvent(0, 0)                         // Call on Event explicitly the first time
}

/*OnRemove called on before removing the client */
func (o *CClient) OnRemove() {
	if o.timer.IsRunning() {
		o.timerw.Stop(&o.timer)
	}
	o.PluginCtx.OnRemove()
}

// OnEvent serves as a callback for the timer, which every 1 sec verifies if the default gateway
// mac is resolved. In case of resolve, it will notify the registered plugins.
func (o *CClient) OnEvent(a, b interface{}) {
	var broadcast bool
	ipv4DGResolved := (o.bitMask & RESOLVED_IPV4_DG_MAC) == RESOLVED_IPV4_DG_MAC
	ipv6DGResolved := (o.bitMask & RESOLVED_IPV6_DG_MAC) == RESOLVED_IPV6_DG_MAC
	if !ipv4DGResolved {
		_, ipv4DGResolved = o.ResolveIPv4DGMac()
		if ipv4DGResolved {
			// Set bit mask that IPv4 was resolved.
			o.bitMask |= RESOLVED_IPV4_DG_MAC
			// A new resolve, we should broadcast
			broadcast = true
		}
	}
	if !ipv6DGResolved {
		_, ipv6DGResolved = o.ResolveIPv6DGMac()
		if ipv6DGResolved {
			// Set bit mask that IPv6 was resolved.
			o.bitMask |= RESOLVED_IPV6_DG_MAC
			// A new resolve, we should broadcast
			broadcast = true
		}
	}
	if broadcast {
		// Should broadcast as something new was resolved.
		o.PluginCtx.BroadcastMsg(nil, MSG_DG_MAC_RESOLVED, o.bitMask, 0)
	}
	if (o.resolveAttempts < o.maxResolveAttempts) && (!ipv4DGResolved || !ipv6DGResolved) {
		// restart ticks as at least of IPv4/IPv6 isn't resolved and we didn't pass the
		// allowed resolve attempts.
		o.resolveAttempts++
		ticks := o.timerw.DurationToTicks(time.Second)
		o.timerw.StartTicks(&o.timer, ticks)
	}
}

// fix this
func (o *CClient) GetIpv6Slaac(l6 *Ipv6Key) bool {
	if o.Ipv6Router == nil {
		return false
	}
	if o.Ipv6Router.PrefixLen == 64 && !o.Ipv6Router.PrefixIpv6.IsZero() {
		copy(l6[:], o.Ipv6Router.PrefixIpv6[:])
		l6[8] = o.Mac[0] ^ 0x2
		l6[9] = o.Mac[1]
		l6[10] = o.Mac[2]
		l6[11] = 0xFF
		l6[12] = 0xFE
		l6[13] = o.Mac[3]
		l6[14] = o.Mac[4]
		l6[15] = o.Mac[5]
		return true
	}
	return false
}

func (o *CClient) GetIpv6LocalLink(l6 *Ipv6Key) {
	l6[0] = 0xFE
	l6[1] = 0x80
	l6[2] = 0
	l6[3] = 0
	l6[4] = 0
	l6[5] = 0
	l6[6] = 0
	l6[7] = 0
	l6[8] = o.Mac[0] ^ 0x2
	l6[9] = o.Mac[1]
	l6[10] = o.Mac[2]
	l6[11] = 0xFF
	l6[12] = 0xFE
	l6[13] = o.Mac[3]
	l6[14] = o.Mac[4]
	l6[15] = o.Mac[5]
}

func (o *CClient) IsValidPrefix(ipv6 Ipv6Key) bool {
	var l6 Ipv6Key
	o.GetIpv6LocalLink(&l6)

	if bytes.Compare(ipv6[0:8], l6[0:8]) == 0 {
		return true
	}

	if o.Ipv6Router != nil {
		if o.Ipv6Router.PrefixLen == 64 {
			if bytes.Compare(o.Ipv6Router.PrefixIpv6[0:8], ipv6[0:8]) == 0 {
				return true
			}
		}
	}
	return false
}

func ExtractMac(ip net.IP, mac *MACKey) bool {
	if len(ip) != net.IPv6len || (ip[0] != 0xfe) && (ip[1]&0xc0 != 0x80) {
		return false
	}
	isEUI48 := ip[11] == 0xff && ip[12] == 0xfe
	if !isEUI48 {
		return false
	}
	mac[0] = ip[8] ^ 2
	mac[1] = ip[9]
	mac[2] = ip[10]
	mac[3] = ip[13]
	mac[4] = ip[14]
	mac[5] = ip[15]
	return true
}

func ExtractOnlyMac(ip net.IP, mac *MACKey) bool {
	if len(ip) != net.IPv6len {
		return false
	}
	isEUI48 := ip[11] == 0xff && ip[12] == 0xfe
	if !isEUI48 {
		return false
	}
	mac[0] = ip[8] ^ 2
	mac[1] = ip[9]
	mac[2] = ip[10]
	mac[3] = ip[13]
	mac[4] = ip[14]
	mac[5] = ip[15]
	return true
}

func (o *CClient) UpdateDgIPv4(NewDgIpv4 Ipv4Key) error {
	old := o.DgIpv4
	o.DgIpv4 = NewDgIpv4
	o.PluginCtx.BroadcastMsg(nil, MSG_UPDATE_DGIPV4_ADDR, old, NewDgIpv4)
	return nil
}

func (o *CClient) UpdateIPv4(NewIpv4 Ipv4Key) error {
	return o.Ns.UpdateClientIpv4(o, NewIpv4)
}

// UpdateIPv6 update static  ipv6
func (o *CClient) UpdateIPv6(NewIpv6 Ipv6Key) error {
	return o.Ns.UpdateClientIpv6(o, NewIpv6)
}

// UpdateDIPv6 update DHCPv6 ipv6
func (o *CClient) UpdateDIPv6(NewIpv6 Ipv6Key) error {
	return o.Ns.UpdateClientDIpv6(o, NewIpv6)
}

// GetL2Header get L2 header
func (o *CClient) GetL2Header(broadcast bool, next uint16) []byte {
	var tund CTunnelData
	o.Ns.Key.Get(&tund)
	b := []byte{}
	if broadcast {
		b = append(b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
	} else {
		b = append(b, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0)
	}
	b = append(b, o.Mac[:]...)
	for i, val := range tund.Vlans {
		if val != 0 {
			val |= (((uint32(o.PbitList[i])) << 13) & 0x0000e000)
			b = append(b, 0, 0, 0, 0)
			binary.BigEndian.PutUint32(b[len(b)-4:], val)
		}
	}
	b = append(b, 0, 0)
	binary.BigEndian.PutUint16(b[len(b)-2:], uint16(next))
	return b
}

func (o *CClient) GetIPv4Header(broadcast bool, next uint8) ([]byte, uint16) {
	l2 := o.GetL2Header(broadcast, uint16(layers.EthernetTypeIPv4))
	offsetIPv4 := uint16(len(l2))
	ipHeader := PacketUtlBuild(
		&layers.IPv4{Version: 4, IHL: 5,
			TTL:      128,
			Id:       0xcc,
			SrcIP:    net.IPv4(o.Ipv4[3], o.Ipv4[2], o.Ipv4[1], o.Ipv4[0]),
			DstIP:    net.IPv4(o.DgIpv4[3], o.DgIpv4[2], o.DgIpv4[1], o.DgIpv4[0]),
			Length:   44,
			Protocol: layers.IPProtocol(next)})
	l2 = append(l2, ipHeader...)
	return l2, offsetIPv4
}

func (o *CClient) IsUnicastToMe(p []byte) bool {

	if len(p) > 6 {
		res := bytes.Compare(o.Mac[0:6], p[0:6])
		if res == 0 {
			return true
		}
	}
	return false
}

func (o *CClient) GetInfo() *CClientInfo {
	var info CClientInfo

	info.Mac = o.Mac
	info.Ipv4 = o.Ipv4
	info.DgIpv4 = o.DgIpv4
	info.MTU = o.MTU

	o.GetIpv6LocalLink(&info.Ipv6Local)
	o.GetIpv6Slaac(&info.Ipv6Slaac)

	info.Ipv6 = o.Ipv6
	info.DgIpv6 = o.DgIpv6
	info.DhcpIpv6 = o.Dhcpv6

	info.Ipv6ForceDGW = o.Ipv6ForceDGW
	info.Ipv6ForcedgMac = o.Ipv6ForcedgMac
	info.ForceDGW = o.ForceDGW
	info.Ipv4ForcedgMac = o.Ipv4ForcedgMac

	info.DGW = o.DGW

	info.Ipv6Router = o.Ipv6Router
	info.Ipv6DGW = o.Ipv6DGW

	info.PlugNames = o.PluginCtx.GetAllPlugNames()

	info.PbitList = o.PbitList

	return &info
}

func (o *CClient) ResolveIPv4DGMac() (mac MACKey, ok bool) {
	if o.ForceDGW {
		mac, ok = o.Ipv4ForcedgMac, true
	} else if o.DGW != nil && o.DGW.IpdgResolved {
		mac, ok = o.DGW.IpdgMac, true
	}
	return mac, ok
}

func (o *CClient) ResolveIPv6DGMac() (mac MACKey, ok bool) {
	if o.Ipv6ForceDGW {
		mac, ok = o.Ipv6ForcedgMac, true
	} else if o.Ipv6DGW != nil && o.Ipv6DGW.IpdgResolved {
		mac, ok = o.Ipv6DGW.IpdgMac, true
	} else if o.Ipv6Router != nil {
		mac, ok = o.Ipv6Router.DgMac, true
	}
	return mac, ok
}

func (o *CClient) GetSourceIPv6() (Ipv6Key, error) {
	if !o.Dhcpv6.IsZero() {
		return o.Dhcpv6, nil
	}
	if !o.Ipv6.IsZero() {
		return o.Ipv6, nil
	}
	var ipv6Slaac Ipv6Key
	if o.GetIpv6Slaac(&ipv6Slaac) {
		return ipv6Slaac, nil
	}
	var key Ipv6Key
	return key, fmt.Errorf(" No IPv6 found for this client! client %v ", o.Mac)
}

func (o *CClient) ResolveSourceIPv6() Ipv6Key {
	if !o.Dhcpv6.IsZero() {
		return o.Dhcpv6
	}
	if !o.Ipv6.IsZero() {
		return o.Ipv6
	}
	var ipv6Slaac Ipv6Key
	if o.GetIpv6Slaac(&ipv6Slaac) {
		return ipv6Slaac
	}
	panic("No IPv6 found for this client!")
}

func (o *CClient) ResolveDGIPv6() (ipv6 Ipv6Key, ok bool) {
	if !o.DgIpv6.IsZero() {
		ipv6, ok = o.DgIpv6, true
	}
	if o.Ipv6Router != nil {
		ipv6, ok = o.Ipv6Router.IPv6, true
	}
	return ipv6, ok
}

func (o *CClient) OwnsIPv6(ipv6 Ipv6Key) bool {
	var ipv6Slaac Ipv6Key
	o.GetIpv6Slaac(&ipv6Slaac)
	var ipv6Local Ipv6Key
	o.GetIpv6LocalLink(&ipv6Local)
	if (ipv6 == o.Dhcpv6) || (ipv6 == o.Ipv6) || (ipv6 == ipv6Slaac) || (ipv6 == ipv6Local) {
		return true
	}
	return false
}

func (o *CClient) ResolveDGv6() (ipv6 Ipv6Key, mac MACKey, ok bool) {
	if !o.DgIpv6.IsZero() {
		// The Ipv6 Default Gateway is assigned, the Mac is either forced or resolved
		if o.Ipv6ForceDGW {
			ipv6, mac, ok = o.DgIpv6, o.Ipv6ForcedgMac, true
		} else if o.Ipv6DGW != nil && o.Ipv6DGW.IpdgResolved {
			ipv6, mac, ok = o.DgIpv6, o.Ipv6DGW.IpdgMac, true
		}
	} else if o.Ipv6Router != nil {
		// The Ipv6 and mac are learned from a RA
		ipv6, mac, ok = o.Ipv6Router.IPv6, o.Ipv6Router.DgMac, true
	}
	return ipv6, mac, ok
}

func (o *CClient) GetIPv6MTU() (mtu uint16) {
	if o.Ipv6Router != nil {
		// Router Advertisement received from the router.
		if o.Ipv6Router.MTU != 0 {
			mtu = o.Ipv6Router.MTU
		} else {
			// Router Solicitation not received yet.
			mtu = 1500
		}
	} else {
		// Unless set, the MTU is the standard MTU of 1500
		mtu = 1500
	}
	return mtu
}

func (o *CClient) IsDGIpv6(ipv6 Ipv6Key) bool {
	if ipv6 == o.DgIpv6 {
		return true
	} else if o.Ipv6Router != nil {
		return ipv6 == o.Ipv6Router.IPv6
	}
	return false
}

func (o *CClient) GetTransportCtx() interface{} {
	return o.transport
}

// set by transport layer
func (o *CClient) SetTransportCtx(val interface{}) {
	o.transport = val
}
