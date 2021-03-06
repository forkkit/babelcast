/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/ice"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v2"
	"github.com/pion/webrtc/v2/pkg/media"
	"github.com/pion/webrtc/v2/pkg/media/samplebuilder"
	"go.nanomsg.org/mangos/v3"
	"go.nanomsg.org/mangos/v3/protocol/sub"

	// register transports
	_ "go.nanomsg.org/mangos/v3/transport/inproc"
)

const maxLate = 50 // number of packets to skip

// channel name should NOT match the negation of valid characters
var channelRegexp = regexp.MustCompile("[^a-zA-Z0-9 ]+")

type Conn struct {
	sync.Mutex

	rtcPeer *WebRTCPeer
	wsConn  *websocket.Conn
	spSock  mangos.Socket

	channelName string
	// store channel name as 4 byte hash
	// used as our pub/sub topic
	spTopic []byte

	errChan       chan error
	infoChan      chan string
	trackQuitChan chan struct{}

	logger *log.Logger

	isPublisher bool
	hasClosed   bool
}

func NewConn(ws *websocket.Conn) *Conn {
	c := &Conn{}
	c.errChan = make(chan error)
	c.infoChan = make(chan string)
	c.trackQuitChan = make(chan struct{})
	c.logger = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)
	// wrap Gorilla conn with our conn so we can extend functionality
	c.wsConn = ws

	return c
}

func (c *Conn) Log(format string, v ...interface{}) {
	id := fmt.Sprintf("WS %x", c.wsConn.RemoteAddr())
	c.logger.Printf(id+": "+format, v...)
}

func (c *Conn) setupSession(ctx context.Context, cmd CmdSession) error {
	var err error

	offer := cmd.SessionDescription
	c.rtcPeer, err = NewPC(offer, c.rtcStateChangeHandler, c.rtcTrackHandler)
	if err != nil {
		return err
	}

	// Sets the LocalDescription, and starts our UDP listeners
	answer, err := c.rtcPeer.pc.CreateAnswer(nil)
	if err != nil {
		return err
	}

	err = c.rtcPeer.pc.SetLocalDescription(answer)
	if err != nil {
		return nil
	}

	j, err := json.Marshal(answer.SDP)
	if err != nil {
		return err
	}
	err = c.writeMsg(wsMsg{Key: "sd_answer", Value: j})
	if err != nil {
		return err
	}

	return nil
}

func (c *Conn) connectPublisher(ctx context.Context, cmd CmdConnect) error {

	if c.rtcPeer == nil {
		return fmt.Errorf("webrtc session not established")
	}

	if cmd.Channel == "" {
		return fmt.Errorf("channel cannot be empty")
	}

	if channelRegexp.MatchString(cmd.Channel) {
		return fmt.Errorf("channel name must contain only alphanumeric characters")
	}

	if publisherPassword != "" && cmd.Password != publisherPassword {
		return fmt.Errorf("incorrect password")
	}

	c.Lock()
	c.channelName = cmd.Channel
	c.Unlock()
	c.Log("setting up publisher for channel '%s'\n", c.channelName)

	c.setTopic(c.channelName)

	c.Lock()
	c.spSock = pubSocket
	c.Unlock()

	if err := reg.AddPublisher(c.channelName); err != nil {
		return err
	}

	return nil
}

func (c *Conn) connectSubscriber(ctx context.Context, cmd CmdConnect) error {
	var err error

	if c.rtcPeer == nil {
		return fmt.Errorf("webrtc session not established")
	}

	//var username string

	if cmd.Channel == "" {
		return fmt.Errorf("channel cannot be empty")
	}
	if channelRegexp.MatchString(cmd.Channel) {
		return fmt.Errorf("channel name must contain only alphanumeric characters")
	}

	c.channelName = cmd.Channel

	c.Log("setting up subscriber for channel '%s'\n", c.channelName)
	c.Lock()
	if c.spSock, err = sub.NewSocket(); err != nil {
		c.Unlock()
		return fmt.Errorf("can't get new sub socket: %s", err)
	}
	c.Unlock()
	if err = c.spSock.Dial("inproc://babelcast/"); err != nil {
		return fmt.Errorf("sub can't dial %s", err)
	}

	c.setTopic(c.channelName)
	c.Lock()
	if err = c.spSock.SetOption(mangos.OptionSubscribe, c.spTopic); err != nil {
		c.Unlock()
		return fmt.Errorf("sub can't subscribe %s", err)
	}
	c.Unlock()

	if err = reg.AddSubscriber(c.channelName); err != nil {
		return err
	}

	go func() {
		defer c.Log("sub read goroutine quitting...\n")
		defer c.Close()

		var data []byte
		for {

			select {
			case <-ctx.Done():
				return
			default:
			}
			if data, err = c.spSock.Recv(); err != nil {
				if err == mangos.ErrClosed {
					c.Log("sub sock recv err: %s\n", err)
					return
				}
				c.errChan <- fmt.Errorf("sub sock recv err %s\n", err)
				continue
			}

			// discard topic data[:4]
			sample := media.Sample{}
			sample.Samples = binary.LittleEndian.Uint32(data[4:8])
			sample.Data = data[8:]

			c.rtcPeer.track.WriteSample(sample)
		}
	}()

	return nil
}

func (c *Conn) Close() {
	c.Lock()
	defer c.Unlock()
	if c.hasClosed {
		return
	}
	if c.trackQuitChan != nil {
		close(c.trackQuitChan)
	}
	if c.rtcPeer != nil {
		c.rtcPeer.Close()
	}
	if c.spSock != nil && !c.isPublisher {
		c.spSock.Close()
	}
	if c.wsConn != nil {
		c.wsConn.Close()
	}
	c.hasClosed = true
}

func (c *Conn) writeMsg(val interface{}) error {
	j, err := json.Marshal(val)
	if err != nil {
		return err
	}
	c.Log("write message %s\n", string(j))
	c.Lock()
	defer c.Unlock()
	if err = c.wsConn.WriteMessage(websocket.TextMessage, j); err != nil {
		return err
	}

	return nil
}

// WebRTC callback function
func (c *Conn) rtcTrackHandler(track *webrtc.Track, receiver *webrtc.RTPReceiver) {
	go func() {
		var err error
		sb := samplebuilder.New(maxLate, &codecs.OpusPacket{})
		defer c.Log("rtcTrackhandler goroutine quitting...\n")
		defer c.Close()
		for {
			select {
			case <-c.trackQuitChan:
				return
			default:
			}
			var p *rtp.Packet
			p, err = track.ReadRTP()
			if err != nil {
				c.errChan <- fmt.Errorf("error reading RTP packet: %s", err)
				continue
			}
			c.Lock()
			if c.spSock == nil {
				// publisher hasn't connected yet
				c.Unlock()
				continue
			}
			c.Unlock()
			// packet goes into samplebuilder, next valid sample comes out
			sb.Push(p)
			sample := sb.Pop()
			if sample == nil {
				continue
			}
			c.Lock()
			// mangoes socket requires []byte where leading bytes is the subscription topic
			buf := bytes.NewBuffer(c.spTopic)
			binary.Write(buf, binary.LittleEndian, sample.Samples)
			buf.Write(sample.Data)
			if err = c.spSock.Send(buf.Bytes()); err != nil {
				if err == mangos.ErrClosed {
					c.Log("sub sock send err: %s\n", err)
					return
				}
				c.errChan <- fmt.Errorf("pub send failed: %s", err)
			}
			c.Unlock()
		}
	}()
}

// WebRTC callback function
func (c *Conn) rtcStateChangeHandler(connectionState webrtc.ICEConnectionState) {

	//var err error

	switch connectionState {
	case ice.ConnectionStateConnected:
		c.Log("ice connected\n")
		c.Log("remote SDP\n%s\n", c.rtcPeer.pc.RemoteDescription().SDP)
		c.Log("local SDP\n%s\n", c.rtcPeer.pc.LocalDescription().SDP)
		c.infoChan <- "ice connected"

	case ice.ConnectionStateDisconnected:
		c.Log("ice disconnected\n")
		c.Close()

		// non blocking channel write, as receiving goroutine may already have quit
		select {
		case c.infoChan <- "ice disconnected":
		default:
		}
	}
}

func (c *Conn) LogHandler(ctx context.Context) {
	defer c.Log("log goroutine quitting...\n")
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-c.errChan:
			j, err := json.Marshal(err.Error())
			if err != nil {
				c.Log("marshal err %s\n", err)
			}
			m := wsMsg{Key: "error", Value: j}
			err = c.writeMsg(m)
			if err != nil {
				c.Log("writemsg err %s\n", err)
			}
			// end the WS session on error
			c.Close()
		case info := <-c.infoChan:
			j, err := json.Marshal(info)
			if err != nil {
				c.Log("marshal err %s\n", err)
			}
			m := wsMsg{Key: "info", Value: j}
			err = c.writeMsg(m)
			if err != nil {
				c.Log("writemsg err %s\n", err)
			}
		}
	}
}

func (c *Conn) PingHandler(ctx context.Context) {
	defer c.Log("ws ping goroutine quitting...\n")
	pingCh := time.Tick(PingInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-pingCh:
			c.Lock()
			// WriteControl can be called concurrently
			err := c.wsConn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(WriteWait))
			if err != nil {
				c.Unlock()
				c.Log("ping client, err %s\n", err)
				return
			}
			c.Unlock()
		}
	}
}

// store a 32bit hash of the channel name in a 4 byte slice
func (c *Conn) setTopic(channelName string) {
	c.Lock()
	c.spTopic = make([]byte, 4)
	binary.LittleEndian.PutUint32(c.spTopic, hash(c.channelName))
	c.Unlock()
}

func hash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}
