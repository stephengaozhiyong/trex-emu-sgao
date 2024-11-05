// Copyright (c) 2020 Cisco Systems and/or its affiliates.
// Licensed under the Apache License, Version 2.0 (the "License");
// that can be found in the LICENSE file in the root of the source
// tree.

package core

/* message  format

uint32 - message header

  MAGIC
  uint16 0xBEEF -- MAGIC FEEB - compress
  uint16 number of packets

each packet is like this

uint8 0xAA -- MAGIC
uint8 vport
uint16 pkt_size

*/

import (
	"bytes"
	"encoding/binary"
	"external/google/gopacket/layers"
	"fmt"
	"io"
	"os"
	"time"

	zmq "github.com/pebbe/zmq4"
)

const (
	ZMQ_PACKET_HEADER_MAGIC = 0xBEEF
	ZMQ_TX_PKT_BURST_SIZE   = 64
	ZMQ_TX_MAX_BUFFER_SIZE  = 32 * 1024
	ZMQ_EMU_IPC_PATH        = "/tmp/emu" // path should be /tmp/emu-port.ipc
)

type VethIFCb interface {
	HandleRxPacket(m *Mbuf)
}

type VethIFZmq struct {
	rxCtx    *zmq.Context
	txCtx    *zmq.Context
	rxSocket *zmq.Socket
	txSocket *zmq.Socket
	rxPort   uint16 // in respect to EMU. rx->emu
	txPort   uint16 // in respect to EMU. emu->tx

	cn          chan []byte
	vec         []*Mbuf
	txVecSize   uint32
	stats       VethStats
	tctx        *CThreadCtx
	K12Monitor  bool     // K12 packet monitoring to monitorDest
	monitorFile *os.File // File to print the K12 packet captured. Default is stdout.
	proxyMode   bool
	cdb         *CCounterDb
	buf         []byte
	cb          VethIFCb
}

func (o *VethIFZmq) SetCb(cb VethIFCb) {
	o.cb = cb
}

func (o *VethIFZmq) CreateSocket(socketStr string) (*zmq.Context, *zmq.Socket) {
	context, err := zmq.NewContext()
	if err != nil || context == nil {
		panic(err)
	}

	socket, err := context.NewSocket(zmq.PAIR)
	if err != nil || socket == nil {
		panic(err)
	}

	if o.proxyMode {
		err = socket.Bind(socketStr)
	} else {
		err = socket.Connect(socketStr)
	}
	if err != nil {
		panic(err)
	}
	return context, socket

}

func (o *VethIFZmq) Create(ctx *CThreadCtx, port uint16, server string, tcp bool, proxyMode bool) {

	var socketStrRx, socketStrTx string
	if tcp {
		socketStrRx = fmt.Sprintf("tcp://%s:%d", server, port)
		socketStrTx = fmt.Sprintf("tcp://%s:%d", server, port+1)
	} else {
		socketStrRx = fmt.Sprintf("ipc://%s-%d.ipc", ZMQ_EMU_IPC_PATH, port)
		socketStrTx = fmt.Sprintf("ipc://%s-%d.ipc", ZMQ_EMU_IPC_PATH, port+1)

	}
	o.proxyMode = proxyMode

	if o.proxyMode {
		o.rxCtx, o.rxSocket = o.CreateSocket(socketStrTx)
		o.txCtx, o.txSocket = o.CreateSocket(socketStrRx)
		o.rxPort = port + 1
		o.txPort = port
	} else {
		o.rxCtx, o.rxSocket = o.CreateSocket(socketStrRx)
		o.txCtx, o.txSocket = o.CreateSocket(socketStrTx)

		o.rxPort = port
		o.txPort = port + 1
	}
	o.buf = make([]byte, 32*1024)

	o.cn = make(chan []byte)

	o.vec = make([]*Mbuf, 0)
	o.txVecSize = 0
	o.tctx = ctx
	o.cdb = NewVethStatsDb(&o.stats)
}

func (o *VethIFZmq) StartRxThread() {
	go o.rxThread()
}

func (o *VethIFZmq) rxThread() {

	for {
		msg, err := o.rxSocket.RecvBytes(0)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			o.stats.RxZmqErr++
		} else {
			o.cn <- msg
		}
	}
}

func (o *VethIFZmq) GetC() chan []byte {
	return o.cn
}

func (o *VethIFZmq) FlushTx() {
	if len(o.vec) == 0 {
		return
	}
	o.buf = o.buf[:0]
	var header uint32
	var pkth [4]byte
	o.stats.TxBatch++
	header = (uint32(0xBEEF) << 16) + uint32(len(o.vec))
	binary.BigEndian.PutUint32(pkth[:], header)
	o.buf = append(o.buf, pkth[:]...) // message header

	for _, m := range o.vec {
		if !m.IsContiguous() {
			panic(" mbuf should be contiguous  ")
		}
		if o.K12Monitor {
			m.DumpK12(o.tctx.GetTickSimInSec(), o.monitorFile)
		}
		var pktHeader uint32
		pktHeader = (uint32(0xAA) << 24) + uint32((m.VPort()&0xff))<<16 + uint32(m.pktLen&0xffff)
		binary.BigEndian.PutUint32(pkth[:], pktHeader)
		o.buf = append(o.buf, pkth[:]...)     // packet header
		o.buf = append(o.buf, m.GetData()...) // packet itself
		m.FreeMbuf()
	}
	o.vec = o.vec[:0]
	o.txVecSize = 0
	o.txSocket.SendBytes(o.buf, 0)
}

func (o *VethIFZmq) Send(m *Mbuf) {

	pktlen := m.PktLen()
	o.stats.TxPkts++
	o.stats.TxBytes += uint64(pktlen)

	if o.txVecSize+pktlen >= ZMQ_TX_MAX_BUFFER_SIZE {
		o.FlushTx()
	}

	if !m.IsContiguous() {
		m1 := m.GetContiguous(&o.tctx.MPool)
		m.FreeMbuf()
		o.vec = append(o.vec, m1)
	} else {
		o.vec = append(o.vec, m)
	}
	o.txVecSize += pktlen
	if len(o.vec) == ZMQ_TX_PKT_BURST_SIZE {
		o.FlushTx()
	}
}

// SendBuffer get a buffer as input, should allocate mbuf and call send
func (o *VethIFZmq) SendBuffer(unicast bool, c *CClient, b []byte, ipv6 bool) {
	var vport uint16
	vport = c.Ns.GetVport()
	m := o.tctx.MPool.Alloc(uint16(len(b)))
	m.SetVPort(vport)
	m.Append(b)
	if unicast {
		var dgMac MACKey
		var ok bool
		if ipv6 {
			dgMac, ok = c.ResolveIPv6DGMac()
		} else {
			dgMac, ok = c.ResolveIPv4DGMac()
		}
		if !ok {
			m.FreeMbuf()
			o.stats.TxDropNotResolve++
			return
		} else {
			p := m.GetData()
			copy(p[6:12], c.Mac[:])
			copy(p[0:6], dgMac[:])
		}
	}
	o.Send(m)
}

// get the packet
func (o *VethIFZmq) OnRx(m *Mbuf) {
	o.stats.RxPkts++
	o.stats.RxBytes += uint64(m.PktLen())
	if o.K12Monitor {
		io.WriteString(o.monitorFile, "\n ->RX<- \n")
		m.DumpK12(o.tctx.GetTickSimInSec(), o.monitorFile)
	}
	if o.proxyMode {
		o.cb.HandleRxPacket(m)
	} else {
		o.tctx.HandleRxPacket(m)
	}
}

/* get the veth stats */
func (o *VethIFZmq) GetStats() *VethStats {
	return &o.stats
}

func (o *VethIFZmq) SimulatorCleanup() {

	for _, m := range o.vec {
		m.FreeMbuf()
	}
	o.vec = nil
	o.rxSocket.Close()
	o.txSocket.Close()
	o.rxCtx.Term()
	o.txCtx.Term()

}

func (o *VethIFZmq) SetDebug(monitor bool, monitorFile *os.File, capture bool) {
	o.K12Monitor = monitor
	o.monitorFile = monitorFile
}

func (o *VethIFZmq) GetCdb() *CCounterDb {
	return o.cdb
}

func (o *VethIFZmq) SimulatorCheckRxQueue() {

}

func (o *VethIFZmq) OnRxStream(stream []byte) {
	o.stats.RxBatch++
	blen := uint32(len(stream))
	if blen < 4 {
		o.stats.RxParseErr++
		return
	}
	header := binary.BigEndian.Uint32(stream[0:4])
	if ((header & 0xffff0000) >> 16) != ZMQ_PACKET_HEADER_MAGIC {
		o.stats.RxParseErr++
		return
	}
	pkts := int(header & 0xffff)
	var of uint16
	of = 4
	var vport uint8
	var pktLen uint16
	var m *Mbuf
	for i := 0; i < pkts; i++ {
		if blen < uint32(of+4) {
			o.stats.RxParseErr++
			return
		}

		header = binary.BigEndian.Uint32(stream[of : of+4])
		if (header & 0xff000000) != 0xAA000000 {
			o.stats.RxParseErr++
			return
		}

		vport = uint8((header & 0x00ff0000) >> 16)
		pktLen = uint16((header & 0x0000ffff))
		if blen < uint32(of+4+pktLen) {
			o.stats.RxParseErr++
			return
		}

		m = o.tctx.MPool.Alloc(pktLen)
		m.SetVPort(uint16(vport))
		slice := stream[of+4 : of+4+pktLen]
		m.Append(slice)
		o.OnRx(m)
		of = of + 4 + pktLen
		useVyos := os.Getenv("USE_VYOS")
		if useVyos != "yes" {
			continue
		}
		if vport != uint8(ToVyosPort) {
			ctk := getTunnelKeyFromSlice(slice, uint16(vport))
			vyosKey := GetCTunnelKeyForVyos(ctk)
			newSlice := setTunnelKeyToSlice(vyosKey, slice)
			ns := CNSCtx{Key: vyosKey}
			c := CClient{Ns: &ns}
			o.SendBuffer(false, &c, newSlice, false)
		} else {
			ctk := getTunnelKeyFromSlice(slice, uint16(vport))
			trexKey, ok := VyosToTrexCTunnelKeyTable[ctk]
			if !ok {
				continue
			}
			newSlice := setTunnelKeyToSlice(trexKey, slice)
			ns := CNSCtx{Key: trexKey}
			c := CClient{Ns: &ns}
			o.SendBuffer(false, &c, newSlice, false)
		}
	}
}

func (o *VethIFZmq) AppendSimuationRPC(request []byte) {
	panic("AppendSimuationRPC should not be called ")
}

func getTunnelKeyFromSlice(p []byte, port uint16) CTunnelKey {
	ctk := CTunnelKey{}
	offset := uint16(14)
	vlanIndex := 0
	var d CTunnelData
	d.Vport = port
	ethHeader := layers.EthernetHeader(p[0:14])
	var nextHdr layers.EthernetType
	nextHdr = layers.EthernetType(ethHeader.GetNextProtocol())
	for {
		if nextHdr == layers.EthernetTypeDot1Q {
			val := binary.BigEndian.Uint32(p[offset-2:offset+2]) & 0xffffffff
			d.Vlans[vlanIndex] = val
			vlanIndex++
			nextHdr = layers.EthernetType(binary.BigEndian.Uint16(p[offset+2 : offset+4]))
			offset += 4
		} else {
			break
		}
	}
	ctk.Set(&d)
	return ctk
}

func setTunnelKeyToSlice(ctk CTunnelKey, p []byte) []byte {
	new := make([]byte, 0)
	offset := uint16(14)
	var d CTunnelData
	ctk.Get(&d)
	ethHeader := layers.EthernetHeader(p[0:14])
	var nextHdr layers.EthernetType
	nextHdr = layers.EthernetType(ethHeader.GetNextProtocol())
	for {
		if nextHdr == layers.EthernetTypeDot1Q {
			nextHdr = layers.EthernetType(binary.BigEndian.Uint16(p[offset+2 : offset+4]))
			offset += 4
		} else {
			break
		}
	}
	// offset是vlan层的结束，ip层的开始，因为offset的前两个字节表示ip层协议，所以需要-2
	afterVlan := p[offset-2:]
	// 生成Vlan字段的字节流
	buffer := bytes.NewBuffer([]byte{})
	for _, val := range d.Vlans {
		if val != 0 {
			binary.Write(buffer, binary.BigEndian, val)
		}
	}
	new = append(new, p[0:12]...)        //Ethernet层，-2个字节的下一层的协议
	new = append(new, buffer.Bytes()...) // vlan层
	new = append(new, afterVlan...)      //ip层协议（2字节）+ ip层
	return new
}
