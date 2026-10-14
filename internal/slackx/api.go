// SPDX-License-Identifier: Apache-2.0

package slackx

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// The response shapes of the published operations, restricted to the fields the connector reads. A
// field Slack adds is ignored; a field it removes fails the fixture that depends on it.

// Channel is one conversation as conversations.list and conversations.info return it.
type Channel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Created    int64  `json:"created"`
	Creator    string `json:"creator"`
	IsArchived bool   `json:"is_archived"`
	IsPrivate  bool   `json:"is_private"`
	IsMember   bool   `json:"is_member"`
	IsShared   bool   `json:"is_ext_shared"`
}

// CreatedAt is the channel's creation instant. Slack states it in whole seconds.
func (c Channel) CreatedAt() time.Time { return time.Unix(c.Created, 0).UTC() }

// ChannelPage is one page of the channel list.
type ChannelPage struct {
	Channels   []Channel
	NextCursor string
}

// ListChannels reads one page of the public channel list, archived channels included: an incident
// channel archived before the first poll is still an incident that was declared.
func (c *Client) ListChannels(ctx context.Context, cursor string, limit int) (ChannelPage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := map[string]string{
		"types": "public_channel", "exclude_archived": "false", "limit": strconv.Itoa(limit),
	}
	if cursor != "" {
		query["cursor"] = cursor
	}
	resp, err := c.Do(ctx, OpConversationsList, query)
	if err != nil {
		return ChannelPage{}, err
	}
	var body struct {
		Channels []Channel `json:"channels"`
		Meta     struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return ChannelPage{}, fmt.Errorf("slackx: conversations.list: %w", err)
	}
	return ChannelPage{Channels: body.Channels, NextCursor: body.Meta.NextCursor}, nil
}

// ChannelInfo reads one channel.
func (c *Client) ChannelInfo(ctx context.Context, id string) (Channel, error) {
	resp, err := c.Do(ctx, OpConversationsInfo, map[string]string{"channel": id})
	if err != nil {
		return Channel{}, err
	}
	var body struct {
		Channel Channel `json:"channel"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return Channel{}, fmt.Errorf("slackx: conversations.info: %w", err)
	}
	return body.Channel, nil
}

// Message is one message as conversations.history returns it. Text is read in memory by the chat
// worker and never leaves it (FR-031, FR-036).
type Message struct {
	TS      string `json:"ts"`
	User    string `json:"user"`
	BotID   string `json:"bot_id"`
	Subtype string `json:"subtype"`
	Text    string `json:"text"`
}

// At is the message instant, from Slack's `ts` (seconds with a microsecond fraction).
func (m Message) At() time.Time {
	sec, frac, _ := cutDot(m.TS)
	s, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return time.Time{}
	}
	var micros int64
	if frac != "" {
		for len(frac) < 6 {
			frac += "0"
		}
		micros, _ = strconv.ParseInt(frac[:6], 10, 64)
	}
	return time.Unix(s, micros*1000).UTC()
}

func cutDot(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// HistoryPage is one page of a channel's history.
type HistoryPage struct {
	Messages   []Message
	HasMore    bool
	NextCursor string
}

// History reads one page of a channel's messages in [oldest, latest). It is refused unless the client
// was built with History.
func (c *Client) History(ctx context.Context, channel string, oldest, latest time.Time, cursor string) (HistoryPage, error) {
	query := map[string]string{"channel": channel, "limit": "200", "inclusive": "false"}
	if !oldest.IsZero() {
		query["oldest"] = SlackTS(oldest)
	}
	if !latest.IsZero() {
		query["latest"] = SlackTS(latest)
	}
	if cursor != "" {
		query["cursor"] = cursor
	}
	resp, err := c.Do(ctx, OpConversationsHistory, query)
	if err != nil {
		return HistoryPage{}, err
	}
	var body struct {
		Messages []Message `json:"messages"`
		HasMore  bool      `json:"has_more"`
		Meta     struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return HistoryPage{}, fmt.Errorf("slackx: conversations.history: %w", err)
	}
	return HistoryPage{Messages: body.Messages, HasMore: body.HasMore, NextCursor: body.Meta.NextCursor}, nil
}

// SlackTS renders an instant the way Slack's `oldest` and `latest` parameters take it.
func SlackTS(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}
