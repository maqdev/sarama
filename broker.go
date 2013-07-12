package kafka

import (
	"io"
	"math"
	"net"
)

type broker struct {
	id   int32
	host *string
	port int32

	correlation_id int32

	conn net.Conn
	addr net.TCPAddr

	requests  chan requestToSend
	responses chan responsePromise
}

type responsePromise struct {
	correlation_id int32
	packets        chan []byte
	errors         chan error
}

type requestToSend struct {
	// we cheat and use the responsePromise channels to avoid creating more than necessary
	response       responsePromise
	expectResponse bool
}

func newBroker(host string, port int32) (b *broker, err error) {
	b = new(broker)
	b.id = -1 // don't know it yet
	b.host = &host
	b.port = port
	err = b.connect()
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (b *broker) connect() (err error) {
	addr, err := net.ResolveIPAddr("ip", *b.host)
	if err != nil {
		return err
	}

	b.addr.IP = addr.IP
	b.addr.Zone = addr.Zone
	b.addr.Port = int(b.port)

	b.conn, err = net.DialTCP("tcp", nil, &b.addr)
	if err != nil {
		return err
	}

	b.requests = make(chan requestToSend)
	b.responses = make(chan responsePromise)

	go b.sendRequestLoop()
	go b.rcvResponseLoop()

	return nil
}

func (b *broker) forceDisconnect(reqRes *responsePromise, err error) {
	reqRes.errors <- err
	close(reqRes.errors)
	close(reqRes.packets)

	close(b.requests)
	close(b.responses)

	b.conn.Close()
}

func (b *broker) encode(pe packetEncoder) {
	pe.putInt32(b.id)
	pe.putString(b.host)
	pe.putInt32(b.port)
}

func (b *broker) decode(pd packetDecoder) (err error) {
	b.id, err = pd.getInt32()
	if err != nil {
		return err
	}

	b.host, err = pd.getString()
	if err != nil {
		return err
	}

	b.port, err = pd.getInt32()
	if err != nil {
		return err
	}
	if b.port > math.MaxUint16 {
		return DecodingError{"Broker port > 65536"}
	}

	err = b.connect()
	if err != nil {
		return err
	}

	return nil
}

func (b *broker) sendRequestLoop() {
	for request := range b.requests {
		buf := <-request.response.packets
		_, err := b.conn.Write(buf)
		if err != nil {
			b.forceDisconnect(&request.response, err)
			return
		}
		if request.expectResponse {
			b.responses <- request.response
		} else {
			close(request.response.packets)
			close(request.response.errors)
		}
	}
}

func (b *broker) rcvResponseLoop() {
	header := make([]byte, 8)
	for response := range b.responses {
		_, err := io.ReadFull(b.conn, header)
		if err != nil {
			b.forceDisconnect(&response, err)
			return
		}

		decoder := realDecoder{raw: header}
		length, _ := decoder.getInt32()
		if length <= 4 || length > 2*math.MaxUint16 {
			b.forceDisconnect(&response, DecodingError{})
			return
		}

		corr_id, _ := decoder.getInt32()
		if response.correlation_id != corr_id {
			b.forceDisconnect(&response, DecodingError{})
			return
		}

		buf := make([]byte, length-4)
		_, err = io.ReadFull(b.conn, buf)
		if err != nil {
			b.forceDisconnect(&response, err)
			return
		}

		response.packets <- buf
		close(response.packets)
		close(response.errors)
	}
}

func (b *broker) sendRequest(req request) (*responsePromise, error) {
	req.correlation_id = b.correlation_id
	packet, err := buildBytes(&req)
	if err != nil {
		return nil, err
	}

	sendRequest := requestToSend{responsePromise{b.correlation_id, make(chan []byte), make(chan error)}, req.expectResponse()}

	b.requests <- sendRequest
	sendRequest.response.packets <- *packet // we cheat to avoid poofing up more channels than necessary
	b.correlation_id++
	return &sendRequest.response, nil
}

// returns true if there was a response, even if there was an error decoding it (in
// which case it will also return an error of some sort)
func (b *broker) sendAndReceive(req request, res decoder) (bool, error) {
	responseChan, err := b.sendRequest(req)
	if err != nil {
		return false, err
	}

	select {
	case buf := <-responseChan.packets:
		// Only try to decode if we got a response.
		if buf != nil {
			decoder := realDecoder{raw: buf}
			err = res.decode(&decoder)
			return true, err
		}
	case err = <-responseChan.errors:
	}

	return false, err
}
