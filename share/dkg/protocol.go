package dkg

import (
	"errors"
	"fmt"
	"time"

	"github.com/drand/kyber"
	"github.com/drand/kyber/sign"
)

type Board interface {
	PushDeals(AuthDealBundle)
	IncomingDeal() <-chan AuthDealBundle
	PushResponses(AuthResponseBundle)
	IncomingResponse() <-chan AuthResponseBundle
	PushJustification(AuthJustifBundle)
	IncomingJustification() <-chan AuthJustifBundle
}

type Phase int

const (
	DealPhase Phase = iota
	ResponsePhase
	JustificationPhase
	FinishPhase
)

type Phaser interface {
	NextPhase() chan Phase
}

type TimePhaser struct {
	out   chan Phase
	sleep func()
}

func NewTimePhaser(p time.Duration) *TimePhaser {
	return NewTimePhaserFunc(func() { time.Sleep(p) })
}

func NewTimePhaserFunc(sleepPeriod func()) *TimePhaser {
	return &TimePhaser{
		out:   make(chan Phase, 4),
		sleep: sleepPeriod,
	}
}

func (t *TimePhaser) Start() {
	t.out <- DealPhase
	t.sleep()
	t.out <- ResponsePhase
	t.sleep()
	t.out <- JustificationPhase
	t.sleep()
	t.out <- FinishPhase
}

func (t *TimePhaser) NextPhase() chan Phase {
	return t.out
}

type Protocol struct {
	board    Board
	phaser   Phaser
	dkg      *DistKeyGenerator
	conf     *Config
	canIssue bool
	res      chan OptionResult
}

type Config struct {
	*DkgConfig
	// Auth is the scheme to use to verify authentication of the packets
	// received from the board. If nil, authentication is not checked.
	Auth sign.Scheme
}

func NewProtocol(c *Config, b Board, phaser Phaser) (*Protocol, error) {
	dkg, err := NewDistKeyHandler(c.DkgConfig)
	if err != nil {
		return nil, err
	}

	return &Protocol{
		board:    b,
		phaser:   phaser,
		dkg:      dkg,
		conf:     c,
		canIssue: dkg.canIssue,
		res:      make(chan OptionResult, 1),
	}, nil
}

func (p *Protocol) Start() {
	var deals []*DealBundle
	var resps []*ResponseBundle
	var justifs []*JustificationBundle
	for {
		select {
		case phase := <-p.phaser.NextPhase():
			switch phase {
			case DealPhase:
				if !p.sendDeals() {
					return
				}
			case ResponsePhase:
				fmt.Printf("proto %d - done sending responses\n", p.dkg.nidx)
				if !p.sendResponses(deals) {
					return
				}
				fmt.Printf("proto %d - done sending responses\n", p.dkg.nidx)
			case JustificationPhase:
				if !p.sendJustifications(resps) {
					return
				}
				fmt.Printf("proto %d - done sending justifications\n", p.dkg.oidx)
			case FinishPhase:
				p.finish(justifs)
				fmt.Printf("proto %d - done \n", p.dkg.nidx)
				return
			}
		case newDeal := <-p.board.IncomingDeal():
			if err := p.VerifySignature(newDeal); err == nil {
				deals = append(deals, newDeal.Bundle)
			}
		case newResp := <-p.board.IncomingResponse():
			if err := p.VerifySignature(newResp); err == nil {
				resps = append(resps, newResp.Bundle)
			}
		case newJust := <-p.board.IncomingJustification():
			if err := p.VerifySignature(newJust); err == nil {
				justifs = append(justifs, newJust.Bundle)
			}
		}
	}
}

// VerifySignature takes the index of the sender of the packet, computes the
// hash and verify if the signature is correct. VerifySignature expects a
// pointer to  an AuthDealBundle, AuthResponseBundle, or AuthJustifBundle.
// It returns nil if the Auth scheme in the config is nil.
func (p *Protocol) VerifySignature(packet interface{}) error {
	if p.conf.Auth == nil {
		return nil
	}
	var ok bool
	var hash []byte
	var pub kyber.Point
	var sig []byte
	switch auth := packet.(type) {
	case AuthDealBundle:
		hash = auth.Bundle.Hash()
		pub, ok = findIndex(p.conf.DkgConfig.OldNodes, auth.Bundle.DealerIndex)
		if !ok {
			return errors.New("no nodes with this public key")
		}
		sig = auth.Signature
	case AuthResponseBundle:
		hash = auth.Bundle.Hash()
		pub, ok = findIndex(p.conf.DkgConfig.NewNodes, auth.Bundle.ShareIndex)
		if !ok {
			return errors.New("no nodes with this public key")
		}
		sig = auth.Signature
	case AuthJustifBundle:
		hash = auth.Bundle.Hash()
		pub, ok = findIndex(p.conf.DkgConfig.OldNodes, auth.Bundle.DealerIndex)
		if !ok {
			return errors.New("no nodes with this public key")
		}
		sig = auth.Signature
	default:
		return errors.New("unknown packet type")
	}

	err := p.conf.Auth.Verify(pub, hash, sig)
	return err
}

type hashable interface {
	Hash() []byte
}

func (p *Protocol) signIt(h hashable) ([]byte, error) {
	msg := h.Hash()
	priv := p.conf.Longterm
	return p.conf.Auth.Sign(priv, msg)
}

func (p *Protocol) sendDeals() bool {
	if !p.canIssue {
		return true
	}
	bundle, err := p.dkg.Deals()
	if err != nil {
		p.res <- OptionResult{
			Error: err,
		}
		return false
	}
	authBundle := AuthDealBundle{
		Bundle: bundle,
	}
	if p.conf.Auth != nil {
		sig, err := p.signIt(bundle)
		if err != nil {
			return false
		}
		authBundle.Signature = sig
	}
	p.board.PushDeals(authBundle)
	fmt.Printf("proto %d - done sending deal\n", p.dkg.oidx)
	return true
}

func (p *Protocol) sendResponses(deals []*DealBundle) bool {
	resp, err := p.dkg.ProcessDeals(deals)
	if err != nil {
		p.res <- OptionResult{
			Error: err,
		}
		// we signal the end since we can't go on
		return false
	}
	if resp != nil {
		authBundle := AuthResponseBundle{
			Bundle: resp,
		}
		if p.conf.Auth != nil {
			sig, err := p.signIt(resp)
			if err != nil {
				return false
			}
			authBundle.Signature = sig
		}
		p.board.PushResponses(authBundle)
	}
	return true
}

func (p *Protocol) sendJustifications(resps []*ResponseBundle) bool {
	res, just, err := p.dkg.ProcessResponses(resps)
	if err != nil {
		p.res <- OptionResult{
			Error: err,
		}
		return false
	}
	if res != nil {
		// we finished
		p.res <- OptionResult{
			Result: res,
		}
		return false
	}
	if just != nil {
		authBundle := AuthJustifBundle{
			Bundle: just,
		}
		if p.conf.Auth != nil {
			sig, err := p.signIt(just)
			if err != nil {
				return false
			}
			authBundle.Signature = sig
		}
		p.board.PushJustification(authBundle)
	}
	return true
}

func (p *Protocol) finish(justifs []*JustificationBundle) {
	res, err := p.dkg.ProcessJustifications(justifs)
	p.res <- OptionResult{
		Error:  err,
		Result: res,
	}
}

func (p *Protocol) WaitEnd() <-chan OptionResult {
	return p.res
}

type OptionResult struct {
	Result *Result
	Error  error
}