package openvpn

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jkroepke/openvpn-auth-oauth2/internal/openvpn/connection"
	"github.com/jkroepke/openvpn-auth-oauth2/internal/utils"
)

// handlePassword enters the password on the OpenVPN management interface connection.
func (c *Client) handlePassword() error {
	buf := make([]byte, 15)

	err := c.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	if err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}

	_, err = c.conn.Read(buf)
	if err != nil {
		return fmt.Errorf("error probe password: %w", err)
	}

	err = c.conn.SetReadDeadline(time.Time{})
	if err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}

	c.logger.Debug(utils.StringConcat("password probe: ", string(buf)))

	switch {
	case string(buf) == "ENTER PASSWORD:":
		if c.conf.OpenVpn.Password == "" {
			return errors.New("management password required")
		}

		if err = c.sendPassword(); err != nil {
			return err
		}
	case c.conf.OpenVpn.Password != "":
		return errors.New("management password expected, but server does not ask for me")
	default:
		// In case there is no password, read the whole line
		c.scanner.Scan()
	}

	return nil
}

// sendPassword enters the password on the OpenVPN management interface connection.
func (c *Client) sendPassword() error {
	if err := c.rawCommand(c.conf.OpenVpn.Password.String()); err != nil {
		return fmt.Errorf("error from password command: %w", err)
	}

	buf := make([]byte, 15)
	if _, err := c.conn.Read(buf); err != nil {
		return fmt.Errorf("unable to read answer after sending password: %w", err)
	}

	if !strings.Contains("SUCCESS: password is correct", string(buf)) { //nolint:gocritic
		return fmt.Errorf("unable to connect to openvpn management interface: %w", ErrInvalidPassword)
	}

	// read the whole line
	c.scanner.Scan()

	return nil
}

// handleMessages handles all incoming messages and route messages to different channels.
func (c *Client) handleMessages() {
	defer close(c.commandResponseCh)
	defer close(c.clientsCh)

	var (
		err error
		buf bytes.Buffer
	)

	buf.Grow(4096)

	for {
		if err = c.readMessage(&buf); err != nil {
			if errors.Is(err, io.EOF) {
				c.logger.Warn("OpenVPN management interface connection terminated")
				c.ctxCancel(nil)

				return
			}

			c.ctxCancel(fmt.Errorf("error reading bytes: %w", err))

			return
		}

		if err = c.handleMessage(buf.String()); err != nil {
			c.ctxCancel(err)
		}
	}
}

func (c *Client) handleMessage(message string) error {
	if message[0] == '>' {
		switch message[0:6] {
		case ">CLIEN":
			return c.handleClientMessage(message)
		case ">HOLD:":
			c.commandsCh <- "hold release"
		case ">INFO:":
			// welcome message
			if strings.HasPrefix(message, ">INFO:OpenVPN Management Interface Version") {
				return nil
			}

			c.commandResponseCh <- message
		default:
			c.writeToPassthroughClient(message)
		}

		return nil
	}

	// SUCCESS: hold release succeeded
	if len(message) >= 13 && message[9:13] == "hold" {
		c.logger.Info("hold release succeeded")

		return nil
	}

	c.commandResponseCh <- message

	return nil
}

func (c *Client) handleClientMessage(message string) error {
	c.logger.Debug(message)

	client, err := connection.NewClient(c.conf, message)
	if err != nil {
		return fmt.Errorf("error parsing client message: %w", err)
	}

	c.clientsCh <- client

	return nil
}

// handlePassword receive a new message from clientsCh and process them.
func (c *Client) handleClients() {
	var (
		client connection.Client
		err    error
	)

	for {
		select {
		case <-c.ctx.Done():
			return // Error somewhere, terminate
		case client = <-c.clientsCh:
			if client.Reason == "" {
				return
			}

			if err = c.processClient(client); err != nil {
				c.ctxCancel(err)

				return
			}
		}
	}
}

// handleCommands receive new command from commandsCh and send them to OpenVPN management interface.
func (c *Client) handleCommands() {
	var command string

	for {
		select {
		case <-c.ctx.Done():
			return // Error somewhere, terminate
		case command = <-c.commandsCh:
			if command == "" {
				return
			}

			if err := c.rawCommand(command); err != nil {
				c.ctxCancel(err)

				return
			}
		}
	}
}
