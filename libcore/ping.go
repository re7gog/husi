package libcore

import (
	"context"
	"net"
	"time"

	"libcore/device"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/urltest"
	E "github.com/sagernet/sing/common/exceptions"
	N "github.com/sagernet/sing/common/network"

	"github.com/xchacha20-poly1305/libping"
)

func IcmpPing(address string, timeout int32) (latency int32, err error) {
	return libping.IcmpPing(address, timeout)
}

func TcpPing(host, port string, timeout int32) (latency int32, err error) {
	address := net.JoinHostPort(host, port)

	start := time.Now()
	conn, err := net.DialTimeout("tcp", address, time.Duration(timeout)*time.Millisecond)
	if err != nil {
		return
	}
	defer conn.Close()

	latency = int32(time.Since(start).Milliseconds())
	return
}

func UrlTest(i *BoxInstance, link string, timeout int32) (latency int32, err error) {
	defer device.DeferPanicToError("box.UrlTest", func(err_ error) { err = err_ })

	var router adapter.Router
	if i == nil {
		// test current
		router = mainInstance.Router()
	} else {
		router = i.Router()
	}

	defOutbound, err := router.DefaultOutbound(N.NetworkTCP)
	if err != nil {
		return 0, E.Cause(err, "find default outbound")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Millisecond)
	defer cancel()

	chLatency := make(chan uint16, 1)
	go func() {
		var t uint16
		t, err = urltest.URLTest(ctx, link, defOutbound)
		chLatency <- t
	}()
	select {
	case <-ctx.Done():
		return 0, E.New("Url test timeout")
	case t := <-chLatency:
		if err != nil {
			return 0, err
		}
		return int32(t), nil
	}
}
