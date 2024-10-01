// Package anchor provides anchor API server.
package anchor

import (
	"net"
	"os"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/buf"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	N "github.com/sagernet/sing/common/network"

	"github.com/xchacha20-poly1305/anchor"
)

var _ adapter.Service = (*Anchor)(nil)

type Anchor struct {
	logger     logger.ContextLogger
	options    Options
	packetConn net.PacketConn
	done       chan struct{}
}

func New(ctxLogger logger.ContextLogger, options Options) (*Anchor, error) {
	packetConn, err := net.ListenUDP(N.NetworkUDP+"4", &net.UDPAddr{
		IP:   net.IPv4zero, // Not listen IPv6
		Port: anchor.Port,
	})
	if err != nil {
		return nil, E.Cause(err, "listen anchor API")
	}
	if ctxLogger == nil {
		ctxLogger = logger.NOP()
	}
	return &Anchor{
		logger:     ctxLogger,
		options:    options,
		packetConn: packetConn,
		done:       make(chan struct{}),
	}, nil
}

func (a *Anchor) Start() error {
	go a.loop()
	return nil
}

func (a *Anchor) loop() {
	a.logger.Info("Anchor: started loop")
	for {
		select {
		case <-a.done:
			return
		default:
		}
		buffer := buf.NewSize(anchor.MaxQuerySize)
		_, source, err := buffer.ReadPacketFrom(a.packetConn)
		if err != nil {
			a.logger.Info("Anchor: stopped because: ", err)
			_ = a.Close()
			return
		}
		go a.handle(source, buffer)
	}
}

func (a *Anchor) handle(source net.Addr, buffer *buf.Buffer) {
	defer buffer.Release()
	a.logger.Debug("Anchor: new query from: ", source)
	query, err := anchor.ParseQuery(buffer.Bytes())
	if err != nil {
		a.logger.Error("Anchor: ", err)
		return
	}
	a.logger.Debug("Anchor: device name: ", query.DeviceName)
	message, _ := (&anchor.Response{
		Version:    anchor.Version,
		DnsPort:    a.options.DnsPort,
		DeviceName: a.options.DeviceName,
		SocksPort:  a.options.SocksPort,
		User:       a.options.User,
	}).MarshalBinary()
	_, err = a.packetConn.WriteTo(message, source)
	if err != nil {
		a.logger.Error("Anchor: ", err)
	}
}

func (a *Anchor) Close() error {
	select {
	case <-a.done:
		return os.ErrClosed
	default:
		close(a.done)
	}
	return common.Close(a.packetConn)
}
