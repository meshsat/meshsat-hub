package main

import (
	"context"

	hubemail "github.com/meshsat/meshsat-hub/internal/email"
	"github.com/meshsat/meshsat-hub/internal/personchat"
	"github.com/meshsat/meshsat-hub/internal/sms"
)

// laneAccounts gives the "@" lane a tenant's own sending accounts, through
// the same pools every other per-tenant sender uses: a customer tenant with
// no Twilio or mail row of its own gets nil, never the platform's
// (MESHSAT-1685).
type laneAccounts struct {
	sms  *sms.ClientPool
	mail *hubemail.Pool
}

func (a laneAccounts) SMS(ctx context.Context, tenantID string) personchat.SMS {
	c := a.sms.ForTenant(ctx, tenantID)
	if c == nil {
		return nil
	}
	return laneSMS{c: c}
}

func (a laneAccounts) Mail(ctx context.Context, tenantID string) (personchat.Mail, string) {
	gw := a.mail.ForTenant(ctx, tenantID)
	if gw == nil || gw.Client == nil {
		return nil, ""
	}
	return gw.Client, gw.ReplyTo
}

type laneSMS struct{ c *sms.Client }

func (l laneSMS) Send(ctx context.Context, to, body string) error {
	_, err := l.c.Send(ctx, to, body)
	return err
}

func (l laneSMS) From() string { return l.c.FromNumber() }
