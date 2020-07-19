/*
 * mjpeg-proxy -- Republish a MJPEG HTTP image stream using a server in Go
 *
 * Copyright (C) 2015, Valentin Vidic
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

/* Sample source stream starts like this:

   HTTP/1.1 200 OK
   Content-Type: multipart/x-mixed-replace;boundary=myboundary
   Cache-Control: no-cache
   Pragma: no-cache

   --myboundary
   Content-Type: image/jpeg
   Content-Length: 36291

   JPEG data...
*/

type Chunker struct {
	url      string
	username string
	password string
	resp     *http.Response
	boundary string
	stop     chan struct{}
}

func NewChunker(url, username, password string) *Chunker {
	chunker := new(Chunker)

	chunker.url = url
	chunker.username = username
	chunker.password = password

	return chunker
}

func (chunker *Chunker) Connect() error {
	fmt.Println("chunker: connecting to", chunker.url)

	req, err := http.NewRequest("GET", chunker.url, nil)
	if err != nil {
		return err
	}

	if chunker.username != "" && chunker.password != "" {
		req.SetBasicAuth(chunker.username, chunker.password)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("request failed: %s", resp.Status)
	}

	boundary, err := getBoundary(*resp)
	if err != nil {
		resp.Body.Close()
		return err
	}

	chunker.resp = resp
	chunker.boundary = boundary
	chunker.stop = make(chan struct{})
	return nil
}

func (chunker *Chunker) GetHeader() http.Header {
	return chunker.resp.Header
}

func (chunker *Chunker) Start(pubChan chan []byte) {
	fmt.Println("chunker: started")

	body := chunker.resp.Body
	reader := bufio.NewReader(body)
	defer func() {
		err := body.Close()
		if err != nil {
			fmt.Println("chunker: body close failed:", err)
		}
	}()
	defer close(pubChan)

	var failure error

ChunkLoop:
	for {
		head, size, err := readChunkHeader(reader)
		if err != nil {
			failure = err
			break ChunkLoop
		}

		data, err := readChunkData(reader, size)
		if err != nil {
			failure = err
			break ChunkLoop
		}

		select {
		case <-chunker.stop:
			break ChunkLoop
		case pubChan <- append(head, data...):
		}

		if size == 0 {
			failure = errors.New("received final chunk of size 0")
			break ChunkLoop
		}
	}

	if failure != nil {
		fmt.Println("chunker: failed: ", failure)
	} else {
		fmt.Println("chunker: stopped")
	}
}

func (chunker *Chunker) Stop() {
	fmt.Println("chunker: stopping")
	close(chunker.stop)
}

func readChunkHeader(reader *bufio.Reader) (head []byte, size int, err error) {
	head = make([]byte, 0)
	size = -1
	err = nil

	// read boundary
	var line []byte
	line, err = reader.ReadSlice('\n')
	if err != nil {
		return
	}

	/* don't check for valid boundary in this function; a lot of webcams
	(such as those by AXIS) seem to provide improper boundaries. */

	head = append(head, line...)

	// read header
	for {
		line, err = reader.ReadSlice('\n')
		if err != nil {
			return
		}
		head = append(head, line...)

		// empty line marks end of header
		lineStr := strings.TrimRight(string(line), "\r\n")
		if len(lineStr) == 0 {
			break
		}

		// find data size
		parts := strings.SplitN(lineStr, ": ", 2)
		if strings.EqualFold(parts[0], "Content-Length") {
			var n int
			n, err = strconv.Atoi(parts[1])
			if err != nil {
				return
			}
			size = n
		}
	}

	if size == -1 {
		err = errors.New("Content-Length chunk header not found")
		return
	}

	return
}

func readChunkData(reader *bufio.Reader, size int) ([]byte, error) {
	buf := make([]byte, size)

	for pos := 0; pos < size; {
		n, err := reader.Read(buf[pos:])
		if err != nil {
			return nil, err
		}

		pos += n
	}

	return buf, nil
}

func getBoundary(resp http.Response) (string, error) {
	ct := strings.Split(resp.Header.Get("Content-Type"), ";")
	fixedCt := ""
	fixedPrefix := "multipart/x-mixed-replace;boundary="

	if len(ct) < 2 || !strings.HasPrefix(ct[0], "multipart/x-mixed-replace") || !strings.HasPrefix(strings.TrimPrefix(ct[1], " "), "boundary=") {
		errStr := fmt.Sprintf("Content-Type is invalid (%s)", strings.Join(ct, ";"))
		return "", errors.New(errStr)
	}
	// Build normalized Content-Type string
	builder := strings.Builder{}
	builder.WriteString(ct[0])
	builder.WriteString(";")
	builder.WriteString(strings.TrimPrefix(ct[1], " "))
	fixedCt = builder.String()

	boundary := "--" + strings.TrimPrefix(fixedCt, fixedPrefix)
	return boundary, nil
}

type PubSub struct {
	chunker     *Chunker
	pubChan     chan []byte
	subChan     chan *Subscriber
	unsubChan   chan *Subscriber
	subscribers map[*Subscriber]bool
}

func NewPubSub(chunker *Chunker) *PubSub {
	pubsub := new(PubSub)

	pubsub.chunker = chunker
	pubsub.subChan = make(chan *Subscriber)
	pubsub.unsubChan = make(chan *Subscriber)
	pubsub.subscribers = make(map[*Subscriber]bool)

	return pubsub
}

func (pubsub *PubSub) Start() {
	go pubsub.loop()
}

func (pubsub *PubSub) Subscribe(s *Subscriber) {
	pubsub.subChan <- s
}

func (pubsub *PubSub) Unsubscribe(s *Subscriber) {
	pubsub.unsubChan <- s
}

func (pubsub *PubSub) loop() {
	for {
		select {
		case data, ok := <-pubsub.pubChan:
			if ok {
				pubsub.doPublish(data)
			} else {
				pubsub.stopChunker()
				pubsub.stopSubscribers()
			}

		case sub := <-pubsub.subChan:
			pubsub.doSubscribe(sub)

		case sub := <-pubsub.unsubChan:
			pubsub.doUnsubscribe(sub)
		}
	}
}

func (pubsub *PubSub) doPublish(data []byte) {
	subs := pubsub.subscribers

	for s := range subs {
		select {
		case s.ChunkChannel <- data: // try to send
		default: // or skip this frame
		}
	}
}

func (pubsub *PubSub) doSubscribe(s *Subscriber) {
	pubsub.subscribers[s] = true

	fmt.Printf("pubsub: added subscriber %s (total=%d)\n",
		s.RemoteAddr, len(pubsub.subscribers))

	if len(pubsub.subscribers) == 1 {
		if err := pubsub.startChunker(); err != nil {
			fmt.Println("pubsub: failed to start chunker:", err)
			pubsub.stopSubscribers()
		}
	}
}

func (pubsub *PubSub) stopSubscribers() {
	for s := range pubsub.subscribers {
		close(s.ChunkChannel)
	}
}

func (pubsub *PubSub) doUnsubscribe(s *Subscriber) {
	delete(pubsub.subscribers, s)

	fmt.Printf("pubsub: removed subscriber %s (total=%d)\n",
		s.RemoteAddr, len(pubsub.subscribers))

	if len(pubsub.subscribers) == 0 {
		pubsub.stopChunker()
	}
}

func (pubsub *PubSub) startChunker() error {
	err := pubsub.chunker.Connect()
	if err != nil {
		return err
	}

	pubsub.pubChan = make(chan []byte)
	go pubsub.chunker.Start(pubsub.pubChan)

	return nil
}

func (pubsub *PubSub) stopChunker() {
	if pubsub.pubChan != nil {
		pubsub.chunker.Stop()
	}

	pubsub.pubChan = nil
}

type Subscriber struct {
	RemoteAddr   string
	ChunkChannel chan []byte
}

func NewSubscriber(client string) *Subscriber {
	sub := new(Subscriber)

	sub.RemoteAddr = client
	sub.ChunkChannel = make(chan []byte)

	return sub
}

func (pubsub *PubSub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// prepare response for flushing
	flusher, ok := w.(http.Flusher)
	if !ok {
		fmt.Printf("server: client %s could not be flushed",
			r.RemoteAddr)
		return
	}

	// subscribe to new chunks
	sub := NewSubscriber(r.RemoteAddr)
	pubsub.Subscribe(sub)
	defer pubsub.Unsubscribe(sub)

	headersSent := false
	for {
		// wait for next chunk
		data, ok := <-sub.ChunkChannel
		if !ok {
			break
		}

		// send header before first chunk
		if !headersSent {
			header := w.Header()
			for k, vv := range pubsub.chunker.GetHeader() {
				for _, v := range vv {
					header.Add(k, v)
				}
			}
			w.WriteHeader(http.StatusOK)
			headersSent = true
		}

		// send chunk to client
		_, err := w.Write(data)
		flusher.Flush()

		// check for client close
		if err != nil {
			fmt.Printf("server: client %s failed: %s\n",
				r.RemoteAddr, err)
			break
		}
	}
}

func main() {
	// check parameters
	source := flag.String("source", "http://example.com/img.mjpg", "source mjpg url")
	username := flag.String("username", "", "source mjpg username")
	password := flag.String("password", "", "source mjpg password")

	bind := flag.String("bind", ":8080", "proxy bind address")
	url := flag.String("url", "/", "proxy serve url")

	flag.Parse()

	// start pubsub client connector
	chunker := NewChunker(*source, *username, *password)
	pubsub := NewPubSub(chunker)
	pubsub.Start()

	// start web server
	fmt.Printf("server: starting on address %s with url %s\n", *bind, *url)
	http.Handle(*url, pubsub)
	err := http.ListenAndServe(*bind, nil)
	if err != nil {
		fmt.Println("server: failed to start:", err)
	}
}
