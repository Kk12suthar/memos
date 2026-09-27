package email

import (
	"context"
	"crypto/tls"
	"net"
	"net/smtp"
	"sync"
	"time"

	"github.com/pkg/errors"
)

const smtpOperationTimeout = 15 * time.Second

// Client represents an SMTP email client.
type Client struct {
	config *Config
}

// NewClient creates a new email client with the given configuration.
func NewClient(config *Config) *Client {
	return &Client{
		config: config,
	}
}

// validateConfig validates the client configuration.
func (c *Client) validateConfig() error {
	if c.config == nil {
		return errors.New("email configuration is required")
	}
	return c.config.Validate()
}

// createAuth creates an SMTP auth mechanism if credentials are provided.
func (c *Client) createAuth() smtp.Auth {
	if c.config.SMTPUsername == "" && c.config.SMTPPassword == "" {
		return nil
	}
	return smtp.PlainAuth("", c.config.SMTPUsername, c.config.SMTPPassword, c.config.SMTPHost)
}

// createTLSConfig creates a TLS configuration for secure connections.
func (c *Client) createTLSConfig() *tls.Config {
	return &tls.Config{
		ServerName: c.config.SMTPHost,
		MinVersion: tls.VersionTLS12,
	}
}

// Send sends an email message via SMTP.
func (c *Client) Send(message *Message) error {
	return c.SendContext(context.Background(), message)
}

// SendContext sends an email message via SMTP and closes the active connection
// when ctx is canceled.
func (c *Client) SendContext(ctx context.Context, message *Message) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Validate configuration
	if err := c.validateConfig(); err != nil {
		return errors.Wrap(err, "invalid email configuration")
	}

	// Validate message
	if message == nil {
		return errors.New("message is required")
	}
	if err := message.Validate(); err != nil {
		return errors.Wrap(err, "invalid email message")
	}

	// Format the message
	body := message.Format(c.config.FromEmail, c.config.FromName)

	// Get all recipients
	recipients := message.GetAllRecipients()

	// Create auth
	auth := c.createAuth()

	// Send based on encryption type
	if c.config.UseSSL {
		return c.sendWithSSLContext(ctx, auth, recipients, body)
	}
	return c.sendWithTLSContext(ctx, auth, recipients, body)
}

// sendWithTLS sends email using STARTTLS (port 587).
func (c *Client) sendWithTLS(auth smtp.Auth, recipients []string, body string) error {
	return c.sendWithTLSContext(context.Background(), auth, recipients, body)
}

func (c *Client) sendWithTLSContext(ctx context.Context, auth smtp.Auth, recipients []string, body string) error {
	serverAddr := c.config.GetServerAddress()

	dialer := &net.Dialer{Timeout: smtpOperationTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", serverAddr)
	if err != nil {
		return errors.Wrapf(err, "failed to connect to SMTP server: %s", serverAddr)
	}
	defer conn.Close()
	stopCloseOnCancel := closeConnectionOnCancel(ctx, conn)
	defer stopCloseOnCancel()
	if err := conn.SetDeadline(smtpDeadline(ctx)); err != nil {
		return errors.Wrap(err, "failed to set SMTP connection deadline")
	}

	client, err := smtp.NewClient(conn, c.config.SMTPHost)
	if err != nil {
		return errors.Wrap(err, "failed to create SMTP client")
	}
	defer client.Quit()

	if c.config.UseTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP server does not support STARTTLS")
		}
		if err := client.StartTLS(c.createTLSConfig()); err != nil {
			return errors.Wrap(err, "failed to start SMTP STARTTLS")
		}
	}

	return c.sendWithClient(client, auth, recipients, body)
}

// sendWithSSL sends email using SSL/TLS (port 465).
func (c *Client) sendWithSSL(auth smtp.Auth, recipients []string, body string) error {
	return c.sendWithSSLContext(context.Background(), auth, recipients, body)
}

func (c *Client) sendWithSSLContext(ctx context.Context, auth smtp.Auth, recipients []string, body string) error {
	serverAddr := c.config.GetServerAddress()

	// Create TLS connection
	tlsConfig := c.createTLSConfig()
	dialer := &net.Dialer{Timeout: smtpOperationTimeout}
	rawConn, err := dialer.DialContext(ctx, "tcp", serverAddr)
	if err != nil {
		return errors.Wrapf(err, "failed to connect to SMTP server with SSL: %s", serverAddr)
	}
	conn := tls.Client(rawConn, tlsConfig)
	defer conn.Close()
	stopCloseOnCancel := closeConnectionOnCancel(ctx, conn)
	defer stopCloseOnCancel()
	if err := conn.SetDeadline(smtpDeadline(ctx)); err != nil {
		return errors.Wrap(err, "failed to set SMTP connection deadline")
	}
	if err := conn.HandshakeContext(ctx); err != nil {
		return errors.Wrap(err, "failed to establish SMTP SSL connection")
	}

	// Create SMTP client
	client, err := smtp.NewClient(conn, c.config.SMTPHost)
	if err != nil {
		return errors.Wrap(err, "failed to create SMTP client")
	}
	defer client.Quit()

	return c.sendWithClient(client, auth, recipients, body)
}

func smtpDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(smtpOperationTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		return ctxDeadline
	}
	return deadline
}

func closeConnectionOnCancel(ctx context.Context, conn net.Conn) func() {
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
	return func() {
		once.Do(func() { close(stop) })
	}
}

func (c *Client) sendWithClient(client *smtp.Client, auth smtp.Auth, recipients []string, body string) error {
	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return errors.Wrap(err, "SMTP authentication failed")
		}
	}

	// Set sender
	if err := client.Mail(c.config.FromEmail); err != nil {
		return errors.Wrap(err, "failed to set sender")
	}

	// Set recipients
	for _, recipient := range recipients {
		if err := client.Rcpt(recipient); err != nil {
			return errors.Wrapf(err, "failed to set recipient: %s", recipient)
		}
	}

	// Send message body
	writer, err := client.Data()
	if err != nil {
		return errors.Wrap(err, "failed to send DATA command")
	}

	if _, err := writer.Write([]byte(body)); err != nil {
		return errors.Wrap(err, "failed to write message body")
	}

	if err := writer.Close(); err != nil {
		return errors.Wrap(err, "failed to close message writer")
	}

	return nil
}
