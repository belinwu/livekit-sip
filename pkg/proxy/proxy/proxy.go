package proxy

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/livekit/protocol/logger"
	"google.golang.org/protobuf/proto"
)

const (
	HeaderPrefix   = "Lk-"
	HeaderCallInfo = HeaderPrefix + "Call-Info"
)

func NewProxy(log logger.Logger, c *Config) (*Proxy, error) {
	p := &Proxy{
		log: log,
		c:   c,
	}
	if err := p.init(); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

type Proxy struct {
	log logger.Logger
	c   *Config

	ua  *sipgo.UserAgent
	srv *sipgo.Server
	cli *sipgo.Client
}

func (p *Proxy) init() error {
	var err error
	p.ua, err = sipgo.NewUA(
		sipgo.WithUserAgentHostname(p.c.SIPHostname),
		sipgo.WithUserAgent("LiveKit Proxy"),
	)
	if err != nil {
		p.log.Errorw("failed to setup user agent", err)
		return err
	}

	p.srv, err = sipgo.NewServer(p.ua)
	if err != nil {
		p.log.Errorw("failed to setup server handle", err)
		return err
	}

	p.cli, err = sipgo.NewClient(p.ua,
		sipgo.WithClientAddr(p.c.InternalIP.String()),
	)
	if err != nil {
		p.log.Errorw("failed to setup client handle", err)
		return err
	}

	p.srv.OnInvite(p.inviteHandler)
	p.srv.OnAck(p.ackHandler)
	p.srv.OnCancel(p.cancelHandler)
	p.srv.OnBye(p.byeHandler)
	return nil
}

func (p *Proxy) ListenAndServe(ctx context.Context) error {
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errc := make(chan error, 3)
	addr := netip.AddrPortFrom(p.c.ListenIP, uint16(p.c.SIPPortListen))
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := p.srv.ListenAndServe(ctx, "udp4", addr.String()); err != nil {
			errc <- err
		}
	}()
	go func() {
		defer wg.Done()
		if err := p.srv.ListenAndServe(ctx, "tcp4", addr.String()); err != nil {
			errc <- err
		}
	}()
	if tconf := p.c.TLS; tconf != nil {
		if len(tconf.Certs) == 0 {
			return errors.New("TLS certificate required")
		}
		var certs []tls.Certificate
		for _, c := range tconf.Certs {
			cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
			if err != nil {
				return err
			}
			certs = append(certs, cert)
		}
		tlsConf := &tls.Config{
			NextProtos:   []string{"sip"},
			Certificates: certs,
		}
		addrTLS := netip.AddrPortFrom(p.c.ListenIP, uint16(tconf.ListenPort))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.srv.ListenAndServeTLS(ctx, "tcp4", addrTLS.String(), tlsConf); err != nil {
				errc <- err
			}
		}()
	}
	select {
	case <-ctx.Done():
		p.log.Infow("shutting down")
		cancel()
		wg.Wait()
		return nil
	case err := <-errc:
		cancel()
		wg.Wait()
		return err
	}
}

func (p *Proxy) Close() {
	if p.cli != nil {
		p.cli.Close()
	}
	if p.srv != nil {
		p.srv.Close()
	}
	if p.ua != nil {
		p.ua.Close()
	}
}

func (p *Proxy) getDestination(req *sip.Request) string {
	return p.c.Destination
}

func (p *Proxy) reply(tx sip.ServerTransaction, req *sip.Request, code sip.StatusCode, reason string) {
	resp := sip.NewResponseFromRequest(req, code, reason, nil)
	resp.SetDestination(req.Source()) //This is optional, but can make sure not wrong via is read
	if err := tx.Respond(resp); err != nil {
		p.log.Errorw("failed to respond on transaction", err)
	}
}

func (p *Proxy) addCustomHeaders(req *sip.Request) {
	p.stripCustomHeaders(req)

	host, sport, _ := net.SplitHostPort(req.Source())
	port, _ := strconv.ParseUint(sport, 10, 16)

	m := &SIPInternalHeader{
		SourceIp:   host,
		SourcePort: uint32(port),
		// TODO: transport
	}
	data, err := proto.Marshal(m)
	if err != nil {
		return
	}
	sdata := base64.StdEncoding.EncodeToString(data)
	req.AppendHeader(sip.NewHeader(HeaderCallInfo, sdata))
}

func (p *Proxy) stripCustomHeaders(m interface{ RemoveHeader(name string) bool }) {
	for m.RemoveHeader(HeaderCallInfo) {
	}
}

func (p *Proxy) route(req *sip.Request, tx sip.ServerTransaction) {
	// If we are proxying to asterisk or other proxy -dst must be set
	// Otherwise we will look on our registration entries
	dst := p.getDestination(req)

	if dst == "" {
		p.reply(tx, req, sip.StatusNotFound, "Not found")
		return
	}

	ctx := context.Background()

	if req.IsInvite() {
		p.addCustomHeaders(req)
	}
	req.SetDestination(dst)
	// Start client transaction and relay our request
	clTx, err := p.cli.TransactionRequest(ctx, req, sipgo.ClientRequestAddVia, sipgo.ClientRequestAddRecordRoute)
	if err != nil {
		p.log.Errorw("RequestWithContext failed", err)
		p.reply(tx, req, sip.StatusInternalServerError, "")
		return
	}
	defer clTx.Terminate()

	// Keep monitoring transactions, and proxy client responses to server transaction
	p.log.Debugw("Starting transaction", "req", req.Method.String())
	for {
		select {
		case <-clTx.Done():
			if err := clTx.Err(); err != nil {
				p.log.Errorw("Client Transaction done with error", err, "req", req.Method.String())
			}
			return

		case m := <-tx.Acks():
			// Acks can not be send directly trough destination
			p.log.Infow("Proxing ACK", "req", req.Method.String(), "dst", dst)
			m.SetDestination(dst)
			p.cli.WriteRequest(m)

		case res, ok := <-clTx.Responses():
			if !ok {
				return
			}
			p.stripCustomHeaders(res)

			res.SetDestination(req.Source())

			// https://datatracker.ietf.org/doc/html/rfc3261#section-16.7
			// Based on section removing via. Topmost via should be removed and check that exist

			// Removes top most header
			res.RemoveHeader("Via")
			if err := tx.Respond(res); err != nil {
				p.log.Errorw("ResponseHandler transaction respond failed", err)
			}

		// Early terminate
		// if req.Method == sip.BYE {
		// 	// We will call client Terminate
		// 	return
		// }

		case <-tx.Done():
			if err := tx.Err(); err != nil {
				if errors.Is(err, sip.ErrTransactionCanceled) {
					// Cancel other side. This is only on INVITE needed
					// We now need a new transaction
					if req.IsInvite() {
						r := newCancelRequest(req)
						res, err := p.cli.Do(ctx, r)
						if err != nil {
							p.log.Errorw("Canceling transaction failed", err, "req", req.Method.String())
							return
						}
						if res.StatusCode != sip.StatusOK {
							p.log.Errorw("Canceling transaction failed with non 200 code", err, "req", req.Method.String())
							return
						}
						return
					}
				}

				p.log.Errorw("Transaction done with error", err, "req", req.Method.String())
				return
			}
			p.log.Debugw("Transaction done", "req", req.Method.String())
			return
		}
	}
}

func (p *Proxy) inviteHandler(req *sip.Request, tx sip.ServerTransaction) {
	p.route(req, tx)
}

func (p *Proxy) ackHandler(req *sip.Request, tx sip.ServerTransaction) {
	dst := p.getDestination(req)
	if dst == "" {
		return
	}
	req.SetDestination(dst)
	if err := p.cli.WriteRequest(req, sipgo.ClientRequestAddVia); err != nil {
		p.log.Errorw("Send failed", err)
		p.reply(tx, req, sip.StatusInternalServerError, "")
	}
}

func (p *Proxy) cancelHandler(req *sip.Request, tx sip.ServerTransaction) {
	p.route(req, tx)
}

func (p *Proxy) byeHandler(req *sip.Request, tx sip.ServerTransaction) {
	p.route(req, tx)
}

func newCancelRequest(invite *sip.Request) *sip.Request {
	req := sip.NewRequest(sip.CANCEL, invite.Recipient)
	req.AppendHeader(sip.HeaderClone(invite.Via())) // Cancel request must match invite TOP via and only have that Via
	req.AppendHeader(sip.HeaderClone(invite.From()))
	req.AppendHeader(sip.HeaderClone(invite.To()))
	req.AppendHeader(sip.HeaderClone(invite.CallID()))
	sip.CopyHeaders("Route", invite, req)
	req.SetSource(invite.Source())
	req.SetDestination(invite.Destination())
	return req
}
