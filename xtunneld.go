package main

import "net"
import "log"
import "container/list"
import "io"
import "sync"
import "bytes"
import "encoding/binary"
import "fmt"
import "time"

const bindAddr = ":2011"
const bufferSize = 4096
const maxConn = 0x10000
const xor = 0x64

type tunnel struct {
    id int
    * list.Element
    send chan []byte
    reply io.Writer
}

type bundle struct {
    t [maxConn] tunnel
    * list.List
    * xsocket
}

type xsocket struct {
    net.Conn
    * sync.Mutex
}

func (s xsocket) Read(data []byte) (n int, err error) {
    n,err = io.ReadFull(s.Conn, data)
    if n>0 {
        for i:=0;i<n;i++ {
            data[i] = data[i]^xor
        }
    }

    return
}

func (s xsocket) Write(data []byte) (n int, err error) {
    s.Lock()
    defer s.Unlock()
    for i:=0;i<len(data);i++ {
        data[i] = data[i]^xor
    }
    x := 0
    all := len(data)

    for all>0 {
        n, err = s.Conn.Write(data)
        if err != nil {
            n += x
            return
        }
        all -= n
        x += n
        data = data[n:]
    }

    return all,err
}



func (t *tunnel) sendClose() {
    buf := [4]byte {
        byte(t.id>>8),
        byte(t.id & 0xff),
        0,
        0,
    }
    t.reply.Write(buf[:])
}

func (t *tunnel) sendBack(buf []byte) {
    buf[0] = byte(t.id>>8)
    buf[1] = byte(t.id & 0xff)
    length := len(buf) - 4
    buf[2] = byte(length >> 8)
    buf[3] = byte(length & 0xff)
    t.reply.Write(buf)
}

func connectSocks() net.Conn {
    c,err := net.DialTCP("tcp", nil, socksAddr)
    if err != nil {
        return nil
    }
    log.Println(c.RemoteAddr())
    return c
}

func (t *tunnel) process() {
    send := t.send
    b,ok := <-send 
    if !ok {
        return
    }
    var addr string
    //sock5代理
    if b[0] == 0x05 {
        //回应确认代理
        t.sendBack([]byte{0x00, 0x00, 0x00, 0x00, 0x05, 0x00})

        b, ok := <-send 
        if !ok {
            log.Println("error while read from send.")
            return
        }
        log.Println(b)
        n := len(b)
        switch b[3] {
        case 0x01:
            //解析代理ip
            type sockIP struct {
                A, B, C, D byte
                PORT       uint16
            }
            sip := sockIP{}
            if err := binary.Read(bytes.NewReader(b[4:n]), binary.BigEndian, &sip); err != nil {
                log.Println("请求解析错误")
                return
            }
            addr = fmt.Sprintf("%d.%d.%d.%d:%d", sip.A, sip.B, sip.C, sip.D, sip.PORT)
        case 0x03:
            //解析代理域名
            host := string(b[5 : n-2])
            var port uint16
            err := binary.Read(bytes.NewReader(b[n-2:n]), binary.BigEndian, &port)
            if err != nil {
                log.Println(err)
                return
            }
            addr = fmt.Sprintf("%s:%d", host, port)
        }

        server, err := net.DialTimeout("tcp", addr, time.Second*3)
        if err != nil {
            log.Println(err)
            t.sendClose()
            return
        }
        //回复确定代理成功
        t.sendBack([]byte{0x00, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
        //转发
        go t.processBack(server)
        io.Copy(server, bytes.NewReader(<-send))
    }
}


func (t *tunnel) processBack(c net.Conn) {
    // c.SetReadDeadline(time.Now().Add(5 * time.Second))
    defer c.Close()
    var buf [bufferSize]byte
    for {
        n,err := c.Read(buf[4:])
        if n>0 {
            t.sendBack(buf[:4+n])
        }
        if n==0 {
            t.sendClose()
            return
        }
        e, ok := err.(net.Error)
        if !(ok && e.Timeout()) && err != nil {
            log.Println(n,err)
            return
        }
    }

}
func (t *tunnel) open(reply io.Writer) {
    t.send = make(chan []byte)
    t.reply = reply
    go t.process()
}

func (t *tunnel) close() {
    close(t.send)
}

func newBundle(clientConn net.Conn) *bundle {
    b := new(bundle)
    b.List = list.New()
    for i:=0;i<maxConn;i++ {
        t := &b.t[i]
        t.id = i
        t.Element = b.PushBack(t)
    }
    b.xsocket = & xsocket { clientConn , new(sync.Mutex) }
    return b
}

func (b *bundle) free(id int) {
    t := &b.t[id]
    if t.Element == nil {
        t.Element = b.PushBack(t)
        t.close()
    }
}

func (b *bundle) get(id int) *tunnel {
    t := &b.t[id]
    if t.Element != nil {
        b.Remove(t.Element)
        t.Element = nil
        t.open(b.xsocket)
    }
    return t
}

func servTunnel(c net.Conn) {
    b := newBundle(c)
    var header [4]byte
    for {
        _,err := b.Read(header[:])
        if err != nil {
            log.Fatal(err)
        }
        id := int(header[0]) << 8 | int(header[1])
        length := int(header[2]) << 8 | int(header[3])
        log.Println("Recv",id,length)
        if length == 0 {
            b.free(id)          
        } else {
            t := b.get(id)
            buf := make([]byte, length)
            _,err := b.Read(buf)
            if err != nil {
                log.Fatal(err)
            }
            t.send <- buf
        }
    }
}

func start(addr string) {
    a , err := net.ResolveTCPAddr("tcp",addr)
    if err != nil {
        log.Fatal(err)
    }
    l , err2 := net.ListenTCP("tcp",a)
    if err2 != nil {
        log.Fatal(err2)
    }
    log.Printf("xtunneld bind %s",a)
    c , err3 := l.Accept()
    if err3 != nil {
        log.Fatal(err3)
    }
    l.Close()
    servTunnel(c)
}

func main() {
    start(bindAddr)
}