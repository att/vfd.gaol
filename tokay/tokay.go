// vi: sw=4 ts=4:
/*
	Mnemonic:	tokay.go
	Abstract:	Tokay listens to the exchange(s) defined in the configuration file
				for VFd requests.  Requests are wrapped into the json that VFd expects
				and then are written to VFd's request pipe (usually /var/log/vfd/pipes/request).
				Tokay maintains a single response pipe over which it expects responses
				back from VFd.  When responses are received, the data is wrapped into
				our advertised response format, and written to the response exchange that
				Tokay created at start.

	Date:		16 March 2018
	Author:		E. Scott Daniels

	Useful links:
			https://godoc.org/github.com/streadway/amqp#example-Channel-Consume
*/

package main

import (
	"bytes"
	"bufio"
	"fmt"
	"flag"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/att/gopkgs/ipc"				// for tickler
	"github.com/streadway/amqp"				// underlying rabbit interface (3rd party)
	"github.com/att/gopkgs/bleater"
	"github.com/att/gopkgs/jsontools"
	"github.com/att/gopkgs/rabbit_hole"		// rabbit MQ things
	"github.com/att/gopkgs/config"			// config file parsing
	"github.com/att/gopkgs/uuid"			// uuid string generator

	"github.com/att/vfd.gaol/tokay/lib/chcom"		// channel comm structs (req/resp)
)

const (
	FL_verbose	uint = 1 << iota
	FL_jdump	
	FL_forreal	
)

var (
	version string
)

/*
	Context passed to collectors for common things
*/
type context struct {
	flags		uint				// FL_constants
	wg 			*sync.WaitGroup 
	synch_ch	chan *chcom.Request	// channel that the serialiser listens to
	resp_ch		chan interface{}	// channel the responder listens to
	req_fifo	 string				// request fifo that VFd is listening on
	resp_fifo	string				// fifo VFd will write reqsponses to
	cdir		string				// configuration directory where .json files are placed for VFd to parse
	sid			string				// our unique sender id

									// things needed for writer
	wr_exch		string				// exchange string for writing (name:type+attrs:key)
	qhost		string
	qport		string				// port RMQ listens on
	pw			string
	uname		string
	rmqw_ch		chan interface{}	// channel the rmq writer listens to
}


// ------- utility --------------------

/*
	Create a unique id that we will add to messages we send.
*/
func gen_sender_id() ( sid string ) {
	h, err := os.Hostname()
	if err != nil {
		h = uuid.NewRandom().String()
	}
	p := os.Getpid()

	return fmt.Sprintf( "%s_%d", h, p )
}

/*
	Build a buffer with a response json that will be sent to the requestor. Has
	the form:
	
		{ 
			msg_key: <str - user disambiguation key>,
			sender: <str -- likely meaningless>,
			state: <str pulled from vfd response>,	
			msg: <msg from tokay, not from VFd when needed (e.g. timeout)>,
			data: {json}
		}

	data and/or msg may be nil.
*/
func build_response( sender string, state string, msg string, msg_key string, data *jsontools.Jtree ) ( jresp string ) {

	if state == "" {
		state = "OK"
	}

	if data != nil {
		jresp = fmt.Sprintf( `{ "sender": %q, "state": %q, "msg": %q, "msg_key": %q, "data": %s }`, sender, state, msg, msg_key, data.Frock() )
	} else {
		jresp = fmt.Sprintf( `{ "sender": %q, "state": %q, "msg": %q, "msg_key": %q }`, sender, state, msg, msg_key )
	}
	
	return jresp
}

/*
	Open a fifo for receiving responses back from VFd.
	Pipe opens block until there is a writer.
*/
func mk_fifo( fname string ) ( fifo *os.File, err error ) {
	var errbuf bytes.Buffer

	fifo, err = os.OpenFile( fname, syscall.O_RDWR, 0664 )		 		// crack it open if there; in rw mode to prevent blocking
	if err == nil {
		return fifo, nil
	}

	err = syscall.Mkfifo( fname, 0660 )									// this is only supported on FreeBSD; if it fails we'll resort to external command
	if err == nil  {
		return os.OpenFile( fname, syscall.O_RDWR, 0664 )		 		// crack it open if there; in rw mode to prevent blocking
	}

	cmd := exec.Command( "mkfifo", "--mode=0666", fname )
	cmd.Stdout = &errbuf
	err = cmd.Run()
	if err != nil {
		return nil, fmt.Errorf( "unable to create fifo: %s: %s", fname, err )
	}

	return os.OpenFile( fname, syscall.O_RDWR, 0664 )		 		// crack it open if there; in rw mode to prevent blocking
}


/*
	Start a writer into the target rabbit and return the struct needed to make 
	use of it.

	NOTE: caller should call defer w.close() to ensure proper clean up when
		their function exits.
*/
func start_rmq_writer( ctx *context, sheep *bleater.Bleater ) ( w *rabbit_hole.Mq_writer ) {
	key := "response"											// default key, needed to create but we will likely never use it
	etype := "direct+ad+!du"									// default type
	exch := "tokay_resp"											// default exchange name

	if ctx.wr_exch != ""  {
		tokens := strings.Split( ctx.wr_exch, ":" )				// exchange-name:type+attrs:key
		switch len( tokens ) {
			case 3:
				key = tokens[2]
				fallthrough

			case 2:
				if( tokens[1] != "" ) {				// unlikely, but allow name::key
					etype = tokens[1]
				}
				fallthrough

			case 1:
				if tokens[0] != "" {
					exch = tokens[0]
				}
		}
	}

	sheep.Baa( 2, "attaching writer to %s@%s:%s ex=%s etype=%s key=%s", ctx.uname, ctx.qhost, "5672", exch, etype, key )
	w, err := rabbit_hole.Mk_mqwriter( ctx.qhost, ctx.qport, ctx.uname, ctx.pw, exch, etype, &key )
	if err != nil {
		sheep.Baa(  0, "abort: unable to attach a writer to %s %s: %s\n", ctx.qhost, exch, err )
		os.Exit( 1 )
	}
	sheep.Baa( 1, "writer attached to %s@%s:%s ex=%s etype=%s key=%s", ctx.uname, ctx.qhost, "5672", exch, etype, key )
	w.Start_writer( key )								// start the writer listening for things to write

	return w
}

/*
	One collector is started for each exchange that we're listening to.  This unpacks the json received and
	passes the map to the goroutine that serialises the requests to VFd. If the jdump option was 
	on in the config file then we dump the raw json to the log in additon to passing it on.
*/
func collector( ctx *context, ch_name string, rdr *rabbit_hole.Mq_reader, master_sheep *bleater.Bleater ) {

	rh_ch := make( chan amqp.Delivery, 4096 )			// our listen channel
	count := 0
	defer rdr.Close()									// ensure reader is closed on return

	sheep := bleater.Mk_bleater( 0, os.Stderr )			// a local sheep to label messages
	sheep.Set_prefix( ch_name )
	master_sheep.Add_child( sheep )						// add to the caller's sheep tree (should force to target file if opened by perent)

	sheep.Baa( 1, "reading from %s", ch_name )

	rdr.Start_eating( rh_ch )
	for {
		select {
			case msg := <- rh_ch:						// wait for next msg from rabbit hole
				count++

				if (ctx.flags & FL_jdump) != 0 {
					if (ctx.flags & FL_verbose) != 0 {
						sheep.Baa( 0, "key=%s body=%s", msg.RoutingKey, msg.Body )
					} else {
						sheep.Baa( 0, "%s", msg.Body )
					}
				}

				jt, err := jsontools.Json2tree( msg.Body ); 				// build a jtree from the json
				if err == nil {
					if (ctx.flags & FL_forreal) != 0 {
						exch_key := jt.Get_string( "exch_key" )			// things we want in the serialiser request
						msg_key := jt.Get_string( "msg_key" )
						if msg_key == nil {
							m := "none-given"
							msg_key = &m
						}

						if exch_key == nil {
							exch_key = &msg.CorrelationId				// user didn't specifically add one, pluck the rabbit id and use that
						}

						req := &chcom.Request {
							Resp_ch:	ctx.rmqw_ch,				// channel where responses are expected to be sent back to rmq
							Source:		ch_name,
							Exch_key:	*exch_key,					// user's exchange level key expected to be used in the rabbit message
							Msg_key:	*msg_key,					// user's disambiguation key
							Rid:		uuid.NewRandom().String(),	// generate a random uuid that we'll send in to avoid dupolication if multiple users send concurrent requests
							Single_use:	false,						// our response channel is multi use and should not be closed
						}

						req.Jtree = jt
						ctx.synch_ch <- req							// send the request on to serialisation
					} else {
						sheep.Baa( 1, "no exec mode set, RMQ message ignored (%d bytes)", len( msg.Body ) )
					}
				} else {
					sheep.Baa( 2, "json parse error: malformed RMQ message received: (%s): %s", msg.Body, err )
				}
		}
	}

								// this code should never be reached
	rdr.Stop()					// turn off listner
	ctx.wg.Done()				// dec counter and possibly release main
	return
}

/*
	Save VF configuration data in the config file. The file is named id.json.
	Returns the filename written to (success only) or error.
*/
func stash_vf_cfg( ctx *context, id *string, config *string ) ( fname string, err error ) {

	fname = fmt.Sprintf( "%s/%s.json", ctx.cdir, *id )
	f, err := os.Create( fname )		// create; truncate if it exists
	if err != nil {
		return "", err
	}
	defer f.Close( )	

	buffer := []byte( *config )			// the config in writable form
	
	for remain := len( buffer ) ; remain > 0; {
		n, err := f.Write( buffer )
		if err != nil {
			return "", err
		}

		remain -= n;
	}

	return fname, nil
}

/*
	Create a request buffer that is sent to VFd on its request fifo.  Vfd currently wants:
		{
			action: add|del|Dump|mirror|ping|verbose|show
			params: {
				filename:	<json filename> 		#for add/del
				resource:	<request data>			# all for show all etc.
				r_fifo:		<response-fifo-name>
				vfd_rid:	<response key>			# our key to match VFd response with a pending block
			}
		}
*/
func mk_vfd_request( action string, fname *string, data *string, fifo string, key *string  ) ( string )  {
	s := "{ "

	s += fmt.Sprintf( `"action": %q,`, action )

	s += fmt.Sprintf( `"params": {` )
		if fname != nil {
			s += fmt.Sprintf( ` "filename": %q,`, *fname )
		}
		if data != nil {
			s += fmt.Sprintf( ` "resource": %q,`, *data )
		}
		s += fmt.Sprintf( ` "r_fifo": %q,`, fifo )
		s += fmt.Sprintf( ` "vfd_rid": %q`, *key )
	s +=  "}"					// end params
	s +=  "}\n\n"				// all requests are double newline terminated

	return s
}


/*
	This listens to a channel for requests generated by the various collectors (rabbit, html etc) 
	and passes them along one at a time to VFd. It does any vetting needed.

	We expect the json in to be of this form:
		{
			action: <action-string>	(add/delete/show...)
			exch_key: <request-id>	 (exchange key used on the reponse)
			msg_key: <string>		 (a private msg key that we will return, but generally ignore)
			target: <string>		 (the target of the action uuid for add/delete, all, pf, etc for show, empty for ping)
			req_data: <stuff> 		 (additional data depending on action type)
				add:  well formed json passed in some shape or form to VFd (NOT a string!!)
				del:  empty
				show: all | pf
		}

		The vfd_req is frocked and then is passed 'as is' to VFd via the config file. 
		is written to a config file and the name of the file is passed inside of a 
		small request on the fifo. The id is the action id; e.g. the virtualisation 
		name/uuid of the external component using the VF.  The vfd_rid is the request 
		id which will be used as the key for messages written to the response exchange. 
		If it is missing then we assume the sender isn't listening for a response and 
		won't send one. We will, however, create a dummy id so that the response back 
		from VFd can be matched and logged. 
*/
func serialiser( ctx *context, master_sheep *bleater.Bleater ) {

	sheep := bleater.Mk_bleater( master_sheep.Get_level(), os.Stderr )			// a local sheep to label messages
	sheep.Set_prefix( "serial" )
	master_sheep.Add_child( sheep )												// add to the caller's sheep tree (should force to target file if opened by perent)
	sheep.Baa( 1, "serialiser is running" )

	fifo, err := os.OpenFile( ctx.req_fifo, syscall.O_RDWR, 0664 )	 			// crack open the pipe to VFd (read/write so we don't block)
	if err != nil {
		sheep.Baa( 0, "abort: unable to open request fifo: %s: %s", ctx.req_fifo, err )
		os.Exit( 1 )
	}
	defer fifo.Close( )
	sheep.Baa( 1, "writing requests to VFd via: %s", ctx.req_fifo )

	for {
		req := <- ctx.synch_ch			// wait for next message (formatted into jtree struct)

		sheep.Baa( 1, "processing request from: %s exch_key=%s msg_key=%s", req.Source, req.Exch_key, req.Msg_key )

		resp := &chcom.Response {			// build response block we will queue with responder so it can match the VFd response
			Exch_key:	req.Exch_key,		// user's request id, not to be confused with the one we gen and send to VFd
			Msg_key:	req.Msg_key,		// the user message level disabmbiguation key (we ignore, but pass round)
			Rid:	req.Rid,				// our request id to match VFd responses back to this
			Wait:	false,					// assume bad case and no response is coming
			Req:	req,					// the original request should we need it later
		}

		fifo_buffer := ""
		sender := req.Jtree.Get_string( "sender" )
		if sender != nil {  
			if *sender == ctx.sid {							// we don't process anything we sent (if it looped back on rabbit)
				continue
			}
		} else {
			dup := "unknown"
			sender = &dup	
		}

		exch_key := &req.Exch_key						// easy reference to the user supplied request/response id
		msg_key := &req.Msg_key							// disambiguation key for user
		vfd_rid := &req.Rid								// the id we use to track message/response between us and VFd
		action := req.Jtree.Get_string( "action" )		// what exactly the requestor desires (add, del, show...)
		target := req.Jtree.Get_string( "target" )		// what we're acting on, or how we're acting (e.g. filename)
		resp.Rdata = build_response( ctx.sid, "ERROR", "request timeout", resp.Msg_key, nil )		// default message when waiting; possibly overwritten below

		if action != nil {
			sheep.Baa( 2, "processing action: %s from %s", *action, *sender )
			reason := ""
			fifo_buffer = ""						// assume nothing to be written onto the fifo

			switch *action {
				case "response":					// no action at the moment; we ignore all responses
	
				case "ping", "dump":				// any action that doesn't have parms is simple
					sheep.Baa( 1, "sending %s", *action )
					fifo_buffer = mk_vfd_request( *action, nil, nil, ctx.resp_fifo, vfd_rid )

				case "Ping":								// internal ping to us, not passed to VFd. build a simple version reqponse to show that the path into this funciton and back works
					sheep.Baa( 1, "responding to Ping: %s", *exch_key )
					resp.Rdata = fmt.Sprintf( `{ "sender": %q, "state": "OK", "msg_key": "%s", "msg": [ "Pong: %s" ] }`, ctx.sid, *msg_key, version )
					
					
				case "add":
					if target != nil {
						data, ok := req.Jtree.Get_subtree( "req_data" )			// data for this is the stuff we dump into the vf config; it is _real_ json in the request, not a string
						if ok {
							vfconfig_str := data.Frock()								// the config for the VF; it's json, so frock it into a string
							fname, err := stash_vf_cfg( ctx, target, &vfconfig_str )	// write the json config info into config directory where VFd can eat it
							if err == nil {
								sheep.Baa( 1, "sending add request stashed in config file: %s", fname )

								fifo_buffer = mk_vfd_request( "add", &fname, nil, ctx.resp_fifo, vfd_rid )		// we just send in the file name
							} else {
								reason = "unable to update config: %s" +  fmt.Sprintf( "%s", err )
							}
						} else {
							reason = "no req_data field in request"
						}
					} else {
						reason = "no target field in request"
					}

				case "del", "delete":
					if target != nil {
						fname := fmt.Sprintf( "%s.json", *target )							// the name that we used to add; VFd probably moved it, so no directory used here
						if fname != "" {
							sheep.Baa( 1, "sending del request with reference name/id: %s", fname )

							fifo_buffer = mk_vfd_request( "delete", &fname, nil, ctx.resp_fifo, vfd_rid )
						} else {
							reason = "unable to update config: %s" +  fmt.Sprintf( "%s", err )
						}
					} else {
						reason = "no target field in request"
					}

				case "mirror":
					data := req.Jtree.Get_string( "req_data" )			// mirror data is the pf vf direction and target-pf
					if data != nil {
						fifo_buffer = mk_vfd_request( *action, nil, data, ctx.resp_fifo, vfd_rid )
					} else {
						reason = "pf/vf/direction/target data missing, or was not a string"
					}

				case "show":
					fifo_buffer = mk_vfd_request( *action, nil, target, ctx.resp_fifo, vfd_rid )

				default:
					reason = "unknown action: " + *action
			}

			if fifo_buffer != "" {								// buffer to push into the fifo is not empty
				nw, err := fifo.Write( []byte( fifo_buffer ) )
				if err == nil {
					resp.Wait = true							// request sent, responder should wait for answer
				} else {
					sheep.Baa( 0, "attempt to write to fifo failed: n=%d %s", nw, err )
					resp.Rdata = build_response( ctx.sid, "ERROR", fmt.Sprintf( "unable to send req: %s", err ), *msg_key, nil )
				}
			} else {
				resp.Rdata = build_response( ctx.sid, "ERROR", fmt.Sprintf( "request dropped: %s", reason ), *msg_key, nil )
			}
		} else {
			sheep.Baa( 1, "no action in request?" )
			continue
		}

		if ctx.resp_ch != nil  {												// if there is a responder channel
			sheep.Baa( 4, "serialiser queues response: %s", resp.Rdata )		// these can be misleading to the casual eye, so only if really chatty
			 ctx.resp_ch <- resp												// send it along
		}
	}

	sheep.Baa( 0, "serialiser is finished and returning" )
	ctx.wg.Done()				// should not get here
}

// ------------------- response processing ----------------------------------------------------------------
/*
	Opens and listens to the response pipe (fifo) from VFd. When a response message is read
	it is written onto the responder channel. This function blocks on the fifo, so it
	shouldn't do anything but just shove the next response along for processing
*/
func resp_reader( ctx *context, master_sheep *bleater.Bleater ) {
	var (
		rerr error
	)

	sheep := bleater.Mk_bleater( 0, os.Stderr )			// a local sheep to label messages
	sheep.Set_prefix( "resp_reader" )
	master_sheep.Add_child( sheep )						// add to the caller's sheep tree (should force to target file if opened by perent)
	sheep.Baa( 1, "resp_reader is running" )

	fifo, err := mk_fifo( ctx.resp_fifo )
	if err != nil {
		sheep.Baa( 0, "abort: unable to open response fifo: %s: %s", ctx.resp_fifo, err )
		os.Exit( 1 )
	}
	defer fifo.Close( )
	sheep.Baa( 0, "respnse fifo opened: %s", ctx.resp_fifo )

	br := bufio.NewReader( fifo )

	for {
		jblob := ""

		for ; rerr == nil; {
			rec, rerr := br.ReadString( '\n' )				// we look for the marker string on a line by itself, so separate on newlines
			if  rerr != nil || rec == "@eom@\n" {			// marker indicates the end
				break
			} else {
				if len( rec ) > 0  {
					sheep.Baa( 4, "resp_reader: added %d bytes to blob", len( rec ) )
					jblob += rec + "\n"
				}
			}
		}

		sheep.Baa( 1, "msg from VFd: %d bytes", len( jblob ) )
		ctx.resp_ch <- []byte( jblob )
	}
}

/*
	Listens to a channel for one of two things: 
		1) json blobs that were received from VFd as responses
		2) responses from the serialiser which can be:
			a) ready to send now (error or local info so no request went to VFd)
			b) waiting for an VFd response blob, so we must queue or match if VFd
				responded before we got the response block.

	When a response is ready to be sent, it is written to the channel
	that is in the resposne block; this is likely a private RMQ exchange
	that was connected to when the request was received.

	This thread will create a writer into the rabbit environment
	and write responses on the exchange (config file) using the 
	request ID as the message key.

	Json that is written to the requestor is:
		msg_key: <disabmbiguation-key-provided-in-request>
		data: { <json data from vfd> }

	The exchange key passed in the requst is placed only in the RMQ header.
*/
func responder( ctx *context, master_sheep *bleater.Bleater ) {

	sheep := bleater.Mk_bleater( 0, os.Stderr )			// a local sheep to label messages
	sheep.Set_prefix( "responder" )
	master_sheep.Add_child( sheep )						// add to the caller's sheep tree (should force to target file if opened by perent)
	sheep.Baa( 1, "responder is running" )

	tch := make( chan *ipc.Chmsg, 1 )					// channel for tickles
	tklr := ipc.Mk_tickler( 2 )
	tklr.Add_spot( 5, tch, 0, nil, 0 )

	pending_resp := make( map[string]*chcom.Response )
	unmatched := make( map[string][]byte )				// msgs received before we see the response block from serialiser
	for {
		var (
			action *string
			vfd_rid *string		// response id from anlois
		)

		select {
			case _ = <-tch:								// a tickle, check for stale requests
				now := time.Now().Unix()
				for _, r := range pending_resp {
					if r.Tstamp < now {
						rdata := fmt.Sprintf( `{ "sender": %q, "state": "ERROR", "msg_key": %q, "msg": "timeout: no response from VFd" }`, ctx.sid, r.Msg_key )
						mqm := &rabbit_hole.Mq_msg {				// a message that allows us to set the key
								Data: []byte( rdata ),
								Key: r.Exch_key,
							}
						r.Req.Resp_ch <- mqm						// just send the immediate response out

						sheep.Baa( 1, "response timed out for request %s", r.Rid )
						delete( pending_resp, r.Rid )
					}
				}

			case stuff := <- ctx.resp_ch:							// block and wait for something
				switch msg := stuff.(type) {
					case []byte:
						jtree, err := jsontools.Json2tree( msg ) 			// blob of bytes expected to be json goo; convert it
						if err == nil {
							action = jtree.Get_string( "action" )
							vfd_rid = jtree.Get_string( "vfd_rid" )
						} else {
							sheep.Baa( 0, "bad response data from VFd: %s", msg )
							break
						}

						if vfd_rid != nil && action != nil {
							switch *action {							// VFd may communicate things other than responses
								case "response":
									resp := pending_resp[*vfd_rid]						// see if we have a request that matches
									if resp != nil {
										state := jtree.Get_string( "state" )			// pull the state out of the VFd message
										msg := jtree.Get_string( "msg" )				// if VFd put a string in, we'll pull it up too, but likley an array of strings which we don't promote
										rbuf := ""
										if msg == nil {
											rbuf = build_response( ctx.sid, *state, "", resp.Msg_key, jtree )		// create a response using the user supplied key, and stuffing in the vfd response as data
										} else {
											rbuf = build_response( ctx.sid, *state, *msg, resp.Msg_key, jtree )
										}

										mqm := &rabbit_hole.Mq_msg {					// a message that allows us to set the key
											Data: []byte( rbuf ),
											Key: resp.Exch_key,							// user's response id is the key
										}
										resp.Req.Resp_ch <- mqm							// send the response to the output channel specified when request sent to serialiser

										delete( pending_resp, *vfd_rid )
										sheep.Baa( 2, "VFd response received, found matching request: vfd_rid=%s", *vfd_rid )
									} else {
										sheep.Baa( 1, "VFd response received, matching request not found: vfd_rid=%s", *vfd_rid )
										unmatched[*vfd_rid] = msg
									}
				
								default:
									sheep.Baa( 1, "unknown action received on response fifo: %s", *action )
							}	
						} else {
							sheep.Baa( 1, "json received with missing id or action: %s", msg )
						}
			
					case *chcom.Response:										// a response block to queue to wait for a  matching VFd response
						sheep.Baa( 1, "responder gets response wait: %v src: (%s) id: %s", msg.Wait, msg.Req.Source, msg.Rid )

						if unmatched[msg.Rid] != nil {							// previous unmatched msg from VFd; queue back on our channel to match this block later
							sheep.Baa( 2, "unmatched response found and was requeued: %s", msg.Rid )
							ctx.resp_ch <- unmatched[msg.Rid]
							delete( unmatched, msg.Rid )
						}

						if msg.Wait {
							msg.Tstamp = time.Now().Unix() + 15				// second granularity is fine here; when this resopnse goes stale
							pending_resp[msg.Rid] = msg						// just tuck the request info away until we have a response

							sheep.Baa( 2, "request awaiting response has been queued for: %s", msg.Rid )
						} else {
							mqm := &rabbit_hole.Mq_msg {				// a message that allows us to set the key
								Data: []byte( msg.Rdata ),
								Key: msg.Exch_key,
							}
							msg.Req.Resp_ch <- mqm						// just send the immediate response out
						}

					default:
						sheep.Baa( 1, "responder unknown message type" )
				}

		}
	}
}


// -----------------------------------------------------------------------------------------------
func main( ) {
	var (
		uname	string = ""
		pw		string = ""
		wg sync.WaitGroup						// wait on each of the collectors we start
		exchange *string
	)

	version = "tokay v1.0/18420"

	cfg_fname	:= flag.String( "c", "/etc/vfd/vfd.cfg", "configuration file" )		// we'll assume a tokay {... } section in vfd
	jdump		:= flag.Bool( "j", false, "dump json to log" )
	no_exec		:= flag.Bool( "n", false, "no-exec" )
	rport		:= flag.String( "P", "5672", "rabbit port" )
	section		:= flag.String( "s", "tokay", "configuration file section" )		// allow for parallel tokey processes and unique sections in the same config
	vlevel		:= flag.Uint( "V", 0, "verbosity level n" )
	verbose		:= flag.Bool( "v", false, "verbosity 1" )
	wants_help	:= flag.Bool( "?", false, "print additional usage details" )
	flag.Parse()

	if *wants_help {
		fmt.Fprintf( os.Stderr, "%s\n", version )
		fmt.Fprintf( os.Stderr, "Request listener for VFd" )
		fmt.Fprintf( os.Stderr, "\n" )

		flag.Usage()

		fmt.Fprintf( os.Stderr, "Only the 'tokay' section in the config file affects this process\n" )
		os.Exit( 0 )
	}
	
	pw = os.Getenv( "TOKAY_RMQPW" )				// environment wins if in config
	uname = os.Getenv( "TOKAY_RMQUNAME" )
	ex_default := "tokay"						// this is ok if there is a private RMQ host for each set of VFd processes, but not if one serves a wide area
	exchange = &ex_default

	if *vlevel <= 0 && *verbose {
		*vlevel = 1
	}

	big_sheep := bleater.Mk_bleater( *vlevel, os.Stderr )		// the master bleater, goroutines will create children to better id output
	big_sheep.Set_prefix( *section )
	big_sheep.Set_level( uint( *vlevel ) )
	big_sheep.Baa( 0, "tokay v1.0 has started" )


	jcfg, err := config.Mk_jconfig( *cfg_fname, "default "  + *section )	// read default and tokay (or -s section) supplied
	if err != nil {
		fmt.Fprintf( os.Stderr, "unable to parse config file: %s: %s", *cfg_fname, err )
		os.Exit( 1 )
	}	

	ctx := &context { 			// set up the context for the collector with defaults, and pick up any cmd line flags
		flags: FL_forreal,
		wg:	&wg,
	}
	ctx.synch_ch = make( chan *chcom.Request, 2048 )
	ctx.sid = gen_sender_id()


	log_dir := jcfg.Extract_stringptr( "tokay default", "log_dir", nil )
	if log_dir != nil {
		big_sheep.Baa( 0, "writing log messages to log file in %s", *log_dir )
		go big_sheep.Sheep_herder( log_dir, 86400 )								// switch to a log file in ldir and roll daily
		big_sheep.Baa( 0, "tokay v1.0 now writing to this log" )
		time.Sleep( time.Duration( 500 ) * time.Millisecond )						// give things a chance to stop wiggling
	} else {
		big_sheep.Baa( 1, "continuing to write messages to stdout: no log dir in config" )
	}

	ctx.req_fifo = jcfg.Extract_string( "tokay default", "vfd_fifo", "/var/lib/vfd/request.fifo" )			// where VFd listens for requests
	ctx.resp_fifo = jcfg.Extract_string( "tokay default", "resp_fifo", "/var/lib/vfd/fifos/tokay.fifo" )	// where we will listen for responses
	ctx.cdir = jcfg.Extract_string( "tokay default", "conf_dir", "/var/lib/vfd/config" )					// where config files are deposited
	cvlevel := jcfg.Extract_int( "tokay default", "verbose", 1 )
	big_sheep.Set_level(  uint( cvlevel ) )

	exchange = nil
	rmq_cfg, err := jcfg.Extract_section( "tokay default", "rabbit", "" )				// get the rabbit section from under tokay or the default; no section, we dont' listen
	if err == nil {
		if pw == "" {																	// pull uname/pass from config if not in env
			pw = rmq_cfg.Extract_string( "default", "mqpw", "" )
		}
		if uname == "" {	
			uname = rmq_cfg.Extract_string( "default", "mquser", "" )
		}
		ctx.qhost = rmq_cfg.Extract_string( "default", "mqhost", "" )
		ctx.qport = rmq_cfg.Extract_string( "default", "mqport", "5672" )
		ctx.wr_exch = rmq_cfg.Extract_string( "default", "resp_exch", "tokay_resp" )			// exchange our writer writes back to
		exchange = rmq_cfg.Extract_stringptr( "default", "req_exch", "tokay_req" )				// main exchange for requests 
	} else {
		big_sheep.Baa( 0, "abort: rabbitMQ section (rabbit) not defined in config file" )
		os.Exit( 1 )
	}

	v := jcfg.Extract_posint( "tokay default", "verbose", 1 )
	big_sheep.Set_level( uint( v ) )

	if pw == "" || uname == ""  {
		big_sheep.Baa( 0, "abort: rabbit username and/or password not defined in the environment (TOKAY_RMQPW, TOKAY_RMQUNAME) or in the tokay section of the config" )
		big_sheep.Baa( 0, "\tpw=%s uname=%s", pw, uname )
		os.Exit( 1 )
	}

	if *verbose {						// some flags are based on command line parms
		ctx.flags |= FL_verbose
	}
	if *jdump {
		ctx.flags |= FL_jdump
	}
	if *no_exec {
		ctx.flags &= ^FL_forreal		// turn off the forreal flag (prevents sending data to VFd)
	}

	ctx.pw = pw										// could have come from env or config; set in context now
	ctx.uname = uname

	ctx.resp_ch = make( chan interface{}, 1024 )	// responder will listen to this for responses from VFd and for queued responses from synch thread
	
	go serialiser( ctx, big_sheep )					// serialise requests (from rabbit collector(s))
	wg.Add( 1 )

	go resp_reader( ctx, big_sheep )				// read responses from VFd
	wg.Add( 1 )

	go responder( ctx, big_sheep )					// match pending responses with VFd data and send to the correct response writer
	wg.Add( 1 )

	if exchange != nil && *exchange != "" {
		etokens := strings.Split( *exchange, "," )		// exchange[:type:key] tokens from -e (this could be zero if no rabbit user/pw defined)
		if len( etokens ) > 0 {
			rwriter := start_rmq_writer( ctx, big_sheep )		// kick the thread that will write back to rmq
			ctx.rmqw_ch = rwriter.Port;							// collectors will insert this in requests passed to serialiser
	
	
			big_sheep.Baa( 2, "connecting to exchanges; adding collectors" )
			for _, exch := range etokens {					// create one collector per exchange
				if( exch == "" ) {
					continue 
				}

				big_sheep.Baa( 1, "token: %s", exch )
				tokens := strings.SplitN( exch, ":", 3 )	// split into 3 exch-name:type+opts:key

				etype := "direct+!du+ad"					// defaults if fields are missing
				ekey := "tokay_req"							// default listen key
				switch len( tokens ) {
					case 2:
						if tokens[1] != "" {
							etype = tokens[1]
						}
	
					case 3:
						if tokens[1] != "" {				// allow name::key
							etype = tokens[1]
						}
						if tokens[2] != "" {				// could be name:type:
							ekey = tokens[2]
						}
				}
	
				big_sheep.Baa( 1, "creating rmq link: %s %s %s ex=%s etype=%s ekey=%s", uname, pw, ctx.qhost, tokens[0], etype, ekey )
				r, err := rabbit_hole.Mk_mqreader( ctx.qhost, *rport, uname, pw, tokens[0], etype, &ekey )		// collector expected to close on return
				if err != nil {
					big_sheep.Baa( 0, "abort: unable to attach a reader for %s: %s", exch, err )
					os.Exit( 1 )
				}
	
				go collector( ctx, tokens[0], r, big_sheep )		// basic collector on each exchange
	
				wg.Add( 1 )
			}
		}
	}

	// chill -- probably forever
	wg.Wait()
	big_sheep.Baa( 0, "main released and is terminating" )
}
