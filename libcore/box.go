package libcore

import (
	"context"
	"net/netip"
	"os"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/common/conntrack"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/clashapi"
	"github.com/sagernet/sing-box/experimental/libbox/platform"
	_ "github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/outbound"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/atomic"
	E "github.com/sagernet/sing/common/exceptions"
	F "github.com/sagernet/sing/common/format"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"

	"libcore/anchor"
	"libcore/protect"
	"libcore/v2rayapilite"

	"github.com/gofrs/uuid/v5"
)

func ResetAllConnections() {
	conntrack.Close()
	log.Debug("Reset system connections done.")
}

// boxState is sing-box state
type boxState = uint8

const (
	boxStateNeverStarted boxState = iota
	boxStateRunning
	boxStateClosed
)

type BoxInstance struct {
	*box.Box
	cancel  context.CancelFunc
	state   atomic.TypedValue[boxState]
	forTest bool

	// closers are libcore extra services' closers.
	closers []any

	protectFun protect.Protect

	selector         *outbound.Selector
	selectorCallback selectorCallback

	clashModeHook     chan struct{}
	clashModeCallback func(mode string)

	pauseManager pause.Manager
	servicePauseFields
}

// NewBoxInstance creates a new BoxInstance.
// If platformInterface is nil, it will use test mode.
func NewBoxInstance(config string, platformInterface PlatformInterface) (b *BoxInstance, err error) {
	forTest := platformInterface == nil
	defer catchPanic("NewSingBoxInstance", func(panicErr error) { err = panicErr })

	options, err := parseConfig(config)
	if err != nil {
		return nil, err
	}

	// create box
	ctx, cancel := context.WithCancel(context.Background())
	ctx = pause.WithDefaultManager(ctx)
	var interfaceWrapper platform.Interface
	if forTest {
		interfaceWrapper = platformInterfaceStub{}
	} else {
		interfaceWrapper = &boxPlatformInterfaceWrapper{
			useProcFS: platformInterface.UseProcFS(),
			iif:       platformInterface,
		}
	}
	boxOption := box.Options{
		Options:           options,
		Context:           ctx,
		PlatformInterface: interfaceWrapper,
	}

	// If set PlatformLogWrapper, box will set something about cache file,
	// which will panic with simple configuration (when URL test).
	if !forTest {
		boxOption.PlatformLogWriter = platformLogWrapper
	}

	instance, err := box.New(boxOption)
	if err != nil {
		cancel()
		return nil, E.Cause(err, "create service")
	}

	b = &BoxInstance{
		Box:     instance,
		cancel:  cancel,
		forTest: forTest,
		protectFun: func(fd int) error {
			return platformInterface.AutoDetectInterfaceControl(int32(fd))
		},
		pauseManager: service.FromContext[pause.Manager](ctx),
	}

	if !forTest {
		// selector
		if proxy, haveProxyOutbound := b.Box.Router().Outbound("proxy"); haveProxyOutbound {
			if selector, isSelector := proxy.(*outbound.Selector); isSelector {
				b.selector = selector
				b.selectorCallback = platformInterface.SelectorCallback
			}
		}

		// Clash
		if clash := b.Box.Router().ClashServer(); clash != nil {
			b.clashModeCallback = platformInterface.ClashModeCallback
		}

		var anchorOptions anchor.Options
		for _, inbound := range options.Inbounds {
			switch inbound.Type {
			case C.TypeMixed:
				listen := (*netip.Addr)(inbound.MixedOptions.Listen)
				if !listen.IsUnspecified() {
					continue
				}
				anchorOptions.SocksPort = inbound.MixedOptions.ListenPort
				users := inbound.MixedOptions.Users
				if len(users) > 0 {
					anchorOptions.User = users[0]
				}
			case C.TypeDirect:
				if inbound.Tag != "dns-in" {
					continue
				}
				anchorOptions.DnsPort = inbound.DirectOptions.OverridePort
			}
		}
		if anchorOptions.SocksPort > 0 && anchorOptions.DnsPort > 0 {
			if host, err := os.Hostname(); err == nil {
				anchorOptions.DeviceName = host
			} else if uuidV1, err := uuid.NewV1(); err == nil {
				anchorOptions.DeviceName = uuidV1.String()
			} else {
				anchorOptions.DeviceName = C.Version
			}
			anchorService, err := anchor.New(log.StdLogger(), anchorOptions)
			if err == nil {
				b.closers = append(b.closers, anchorService)
			}
		}
	}

	return b, nil
}

func (b *BoxInstance) Start() (err error) {
	defer catchPanic("box.Start", func(panicErr error) { err = panicErr })

	if b.state.Load() != boxStateNeverStarted {
		return E.New("box already started")
	}

	b.state.Store(boxStateRunning)
	err = b.Box.Start()
	if err != nil {
		return err
	}

	if !b.forTest {
		if b.selector != nil {
			oldCancel := b.cancel
			ctx, cancel := context.WithCancel(context.Background())
			b.cancel = func() {
				oldCancel()
				cancel()
			}
			go b.listenSelectorChange(ctx, b.selectorCallback)
		}

		anchorService := common.Find(b.closers, func(it any) bool {
			if it == nil {
				return false
			}
			_, isAnchor := it.(*anchor.Anchor)
			return isAnchor
		})
		if anchorService != nil {
			_ = anchorService.(*anchor.Anchor).Start()
		}
		if closer := protect.ServerProtect(ProtectPath, b.protectFun); closer != nil {
			b.closers = append(b.closers, closer)
		}
	}

	return nil
}

func (b *BoxInstance) Close() (err error) {
	return b.CloseTimeout(C.FatalStopTimeout)
}

func (b *BoxInstance) CloseTimeout(timeout time.Duration) (err error) {
	defer catchPanic("BoxInstance.Close", func(panicErr error) { err = panicErr })

	// no double close
	if b.state.Swap(boxStateClosed) == boxStateClosed {
		return nil
	}

	_ = common.Close(b.closers...)
	if b.clashModeHook != nil {
		select {
		case <-b.clashModeHook:
			// closed
		default:
			b.Router().ClashServer().(*clashapi.Server).SetModeUpdateHook(nil)
			close(b.clashModeHook)
		}
	}

	// close box
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	go func(done chan<- struct{}) {
		defer catchPanic("box.Close", func(panicErr error) { err = panicErr })
		b.cancel()
		_ = b.Box.Close()
		close(done)
	}(done)
	select {
	case <-ctx.Done():
		return E.New("sing-box did not close in time")
	case <-done:
		log.Info("sing-box closed in ", F.Seconds(time.Since(start).Seconds()), " s.")
		return nil
	}
}

func (b *BoxInstance) NeedWIFIState() bool {
	return b.Box.Router().NeedWIFIState()
}

func (b *BoxInstance) QueryStats(tag, direct string) int64 {
	statsGetter := b.Router().V2RayServer().(v2rayapilite.StatsGetter)
	if statsGetter == nil {
		return 0
	}
	return statsGetter.QueryStats("outbound>>>" + tag + ">>>traffic>>>" + direct)
}

func (b *BoxInstance) SelectOutbound(tag string) (ok bool) {
	if b.selector != nil {
		return b.selector.SelectOutbound(tag)
	}
	return false
}
