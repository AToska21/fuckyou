package place

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"net/http"
	"path"
	"strconv"
	"sync"
	"time"
        "strings"
        "net"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  64,
	WriteBufferSize: 64,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	Error: func(w http.ResponseWriter, req *http.Request, status int, err error) {
		log.Println(err)
		http.Error(w, "Error while trying to make websocket connection.", status)
	},
}

type Server struct {
	sync.RWMutex
	msgs        chan []byte
	close       chan int
	clients     []chan []byte
	img         draw.Image
	imgBuf      []byte
	currentReq  *http.Request // New field to store the current HTTP request
}

func NewServer(img draw.Image, count int) *Server {
	sv := &Server{
		RWMutex:    sync.RWMutex{},
		msgs:       make(chan []byte),
		close:      make(chan int),
		clients:    make([]chan []byte, count),
		img:        img,
	}
	go sv.broadcastLoop()
	return sv
}

func (sv *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch path.Base(req.URL.Path) {
	case "place.png":
		sv.HandleGetImage(w, req)
	case "stat":
		sv.HandleGetStat(w, req)
	case "ws":
		sv.HandleSocket(w, req)
	default:
		http.Error(w, "Not found.", 404)
	}
}

func (sv *Server) HandleGetImage(w http.ResponseWriter, req *http.Request) {
	b := sv.GetImageBytes() //not thread safe but it won't do anything bad
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Write(b)
}

func (sv *Server) HandleGetStat(w http.ResponseWriter, req *http.Request) {
	count := 0
	for _, ch := range sv.clients {
		if ch != nil {
			count++
		}
	}
	fmt.Fprint(w, count)
}

func (sv *Server) HandleSocket(w http.ResponseWriter, req *http.Request) {
	sv.Lock()
	defer sv.Unlock()
	i := sv.getConnIndex()
	if i == -1 {
		log.Println("Server full.")
		http.Error(w, "Server full.", 503)
		return
	}
	sv.currentReq = req // Store the current HTTP request in the Server struct
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		log.Println(err)
		return
	}
	ch := make(chan []byte, 8)
	sv.clients[i] = ch
	go sv.readLoop(conn, i)
	go sv.writeLoop(conn, ch)
}

func (sv *Server) getConnIndex() int {
	for i, client := range sv.clients {
		if client == nil {
			return i
		}
	}
	return -1
}

func rateLimiter() func() bool {
	const maxRequests = 6    // maximum requests per second
	const banDuration = time.Minute // 1 minute ban duration

	var last time.Time
	var requestCount int
	return func() bool {
		now := time.Now()
		if now.Sub(last) < time.Second {
			requestCount++
			if requestCount > maxRequests {
				return true // Kick the client for exceeding the rate limit
			}
		} else {
			last = now
			requestCount = 1
		}
		return false
	}
}

func (sv *Server) readLoop(conn *websocket.Conn, i int) {
	limiter := rateLimiter()
	for {
		_, p, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if limiter() {
			log.Println("Client kicked for high rate.")
			break
		}
		if sv.handleMessage(p) != nil {
			log.Println("Client kicked for bad message.")
			break
		}
	}
	sv.close <- i
}


func (sv *Server) writeLoop(conn *websocket.Conn, ch chan []byte) {
	for {
		if p, ok := <-ch; ok {
			conn.WriteMessage(websocket.BinaryMessage, p)
		} else {
			break
		}
	}
	conn.Close()
}

func (sv *Server) handleMessage(p []byte) error {
	// Get the real client's IP address from the X-Forwarded-For header or the RemoteAddr.
	var ip string
	if xForwardedFor := sv.currentReq.Header.Get("X-Forwarded-For"); xForwardedFor != "" {
		ips := strings.Split(xForwardedFor, ",")
		ip = strings.TrimSpace(ips[0])
	} else {
		ip, _, _ = net.SplitHostPort(sv.currentReq.RemoteAddr)
	}

	x, y, c := parseEvent(p)

	if !sv.setPixel(x, y, c) {
		return errors.New("invalid placement")
	}

	// Log the pixel placement with the color and the IP address of the placer.
	log.Printf("Pixel placed at (%d, %d) with color %v by IP: %s", x, y, c, ip)

	sv.msgs <- p
	return nil
}

func (sv *Server) broadcastLoop() {
	for {
		select {
		case i := <-sv.close:
			if sv.clients[i] != nil {
				close(sv.clients[i])
				sv.clients[i] = nil
			}
		case p := <-sv.msgs:
			for i, ch := range sv.clients {
				if ch != nil {
					select {
					case ch <- p:
					default:
						close(ch)
						sv.clients[i] = nil
					}
				}
			}
		}
	}
}

func (sv *Server) GetImageBytes() []byte {
	if sv.imgBuf == nil {
		buf := bytes.NewBuffer(nil)
		if err := png.Encode(buf, sv.img); err != nil {
			log.Println(err)
		}
		sv.imgBuf = buf.Bytes()
	}
	return sv.imgBuf
}

func (sv *Server) setPixel(x, y int, c color.Color) bool {
	rect := sv.img.Bounds()
	width := rect.Max.X - rect.Min.X
	height := rect.Max.Y - rect.Min.Y
	if 0 > x || x >= width || 0 > y || y >= height {
		return false
	}
	sv.img.Set(x, y, c)
	sv.imgBuf = nil
	return true
}

func parseEvent(b []byte) (int, int, color.Color) {
	if len(b) != 11 {
		return -1, -1, nil
	}
	x := int(binary.BigEndian.Uint32(b))
	y := int(binary.BigEndian.Uint32(b[4:]))
	return x, y, color.NRGBA{b[8], b[9], b[10], 0xFF}
}
