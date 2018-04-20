// vi: sw=4 ts=4:
/*
	Mnemonic:	tokay_req.go
	Abstract:	Accepts a VFd request that should be sent via RabbitMQ to tokay which is expected
				to forward the request to VFd. The response json received from tokay is written to
				standard out as is.  This is somewhat specialised as it needs to know how to format
				the various VFd requests; tokay is only a passthrough expecting the sender to know
				exactly how to build a VFd request.

	Date:		20 March 2018
	Author:		E. Scott Daniels

	Useful links:
			https://godoc.org/github.com/streadway/amqp#example-Channel-Consume
*/

package main

import (
	"fmt"
	"flag"
	"os"
	"strings"
	"sync"

	"github.com/streadway/amqp"
	"github.com/att/gopkgs/jsontools"
	"github.com/att/gopkgs/bleater"
	"github.com/att/gopkgs/rabbit_hole"
	"github.com/att/gopkgs/uuid"
)

const (
	Rbuf_len	int = 1024 * 32			// this should be plenty of space for the response
)

var (
	key_counter int = 0					// keep random string unique
	resp_key string = "no-key"			// key we look for on the response exchange
)

// -----------------------------------------------------------------------------------------------
/*
	Make a random key for a message.
*/
func gen_key( ) ( string ) {
	key_counter += 1
	return fmt.Sprintf( "%s-%d", uuid.NewRandom().String(), key_counter )
}

/*
	Run as a goroutine, this waits for messages from the other side rabbit reader (rdr) and
	does something with what it receives. When we exit, we signal our finishing on the wait
	group so that the main process can exit.
*/
func collector( ch_name string, rdr *rabbit_hole.Mq_reader, num int, raw_json bool,  wg *sync.WaitGroup, sheep *bleater.Bleater ) {

	rh_ch := make( chan amqp.Delivery, 4096 )			// our listen channel
	count := 0
	defer rdr.Close()									// ensure reader is closed on return

	sheep.Baa( 1, "reading from tokay response exchange: %s", ch_name )
	
	rdr.Start_eating( rh_ch )
	for {
		msg := <- rh_ch									// wait for next msg from rabbit hole
		if raw_json {
			fmt.Printf( "%s\n", msg.Body )
		} else {
			jt, _ := jsontools.Json2tree( msg.Body )
			jt.Pretty_print( os.Stdout )
		}

		msg.Body = nil
		count++
		if( num > 0 && count >= num ) {
			break
		}

	}

	rdr.Stop()					// turn off listner
	wg.Done()					// dec counter and possibly release main
	return
}

// -------------- request builders ---------------------------------
/*
	These functions all accept an 'argv' array. [0] is assumed to be the request name
	(e.g. show or add), and postitional parms (probably from the command line) are
	expected to following in elements 1..n.  Tokay expects two types of request parameter
	fields:  target, req_data.  Target is the target of the operation (e.g. file name for
	add/del, or "all," "pfs," etc. for show; it is typically a single token.  Strings contiaining
	multiple tokens, or raw json (unescaped), are passed using the req_data field. This includes
	the string of mirror parms as well as the json for add.
*/

/*
	Generate an add request, argv[1] is expected to be target name (port id, or what ever will be used
	as the .json file name.  Argv[2] is expected to be the configureation jason  that we will slam in as is
	into the req_data field. 
	We could parse and reject if not valid, but whee is the fun in that?  
*/
func mk_add( argv []string ) ( string ) {
	if len( argv ) < 3  {
		return ""
	}

	return fmt.Sprintf( `{ "action": "add", "exch_key": %q, "msg_key": %q, "target": %q, "req_data": %s }`, resp_key, gen_key(), argv[1], argv[2] )
}

/*
	Generate a delete request. Argv[1] is expected to be the name given on an add request
	previously submitted to VFd.
*/
func mk_del( argv []string ) ( string ) {
	if len( argv ) < 2  {
		return ""
	}

	return fmt.Sprintf( `{ "action": "delete", "exch_key": %q, "msg_key": %q, "target": %q }`, resp_key, gen_key(), argv[1] )
}

/*
	Generate a dump request. Response from this is just an ack; dump output goes into VFd log, so direct
	access to the system is needed.
*/
func mk_dump( argv []string ) ( string ) {

	return fmt.Sprintf( `{ "action": "dump", "exch_key": %q, "msg_key": %q, "target": "" }`, resp_key, gen_key() )
}

/*
	Generate a show request. Argv[1] 
*/
func mk_show( argv []string ) ( string ) {
	show_type := "all"				// default to all.

	if len( argv ) > 1 {
		show_type = argv[1]
	}
	return fmt.Sprintf( `{ "action": "show", "exch_key": %q, "msg_key": %q, "target": %q }`, resp_key, gen_key(), show_type )
}

/*
	Generate a ping request. The kind of ping (Ping or ping) is pulled from the command args.
*/
func mk_ping( argv []string ) ( string ) {
	return fmt.Sprintf( `{ "action": %q, "exch_key": %q, "msg_key": %q, "req_data": "" }`, argv[0], resp_key, gen_key() )
}

/*
	Generate a verbose request.
*/
func mk_verbose( argv []string ) ( string ) {
	if len( argv ) > 1 {
		return fmt.Sprintf( `{ "action": "verbose", "exch_key": %q, "msg_key": %q, "req_data": "%s" }`, resp_key, gen_key(), argv[1] )
	} else {
		return ""
	}
}

/*
	Generate a mirror request. Argv[1] we assume is a string which contains:
		<vf> <pf> <direction> [<target>]

	The string is passed to tokay/VFd as is in request data. 
*/
func mk_mirror( argv []string ) ( string ) {
	data := argv[1]
	if len( argv ) > 2 {		// assume each given as separate, bang together
		for i := 2; i < len( argv ); i++ {
			data += " " + argv[i]
		}
	}
		
	return fmt.Sprintf( `{ "action": "mirror", "exch_key": %q, "msg_key": %q, "req_data": %q }`, resp_key, gen_key(), data )
}

// -----------------------------------------------------------------------------------------------

func main( ) {
	var (
		uname	string = ""
		pw		string = ""
		err		error
		wg sync.WaitGroup						// wait on each of the collectors we start
	)

	exchange	:= flag.String( "e", "tokay_req", "exchange tokay is listening on (can be given as exname:type+ops:key)" )
	ex_host		:= flag.String( "h", "localhost", "host where RabbitMQ is running" )
	raw_json	:= flag.Bool( "j", false, "raw json output" )
	rmqport		:= flag.String( "p", "5672", "Rabbit MQ port" )
	rexch		:= flag.String( "r", "tokay_resp", "exchange tokay will write to" )

	vlevel		:= flag.Uint( "V", 0, "verbosity level n" )
	verbose		:= flag.Bool( "v", false, "verbosity 1" )
	wants_help	:= flag.Bool( "?", false, "print additional usage details" )
	flag.Parse()
	argv := flag.Args()		// positional arguments

	if *wants_help || len( argv ) <= 0 {
		fmt.Fprintf( os.Stderr, "tokay_req V1.0/18320\n\n" )
		flag.Usage()

		fmt.Fprintf( os.Stderr, "\n" )
		fmt.Fprintf( os.Stderr, "exchange types:  direct, fanout, topic\n" )
		fmt.Fprintf( os.Stderr, "exchange opts:  ad | !ad  (autodelete)\n" )
		fmt.Fprintf( os.Stderr, "exchange opts:  du | !du  (durable)\n" )
		fmt.Fprintf( os.Stderr, "exchange options are separated from type, and each other, by a plus sign (+)\n" )
		fmt.Fprintf( os.Stderr, "\nRMQ_UNAME and RMQ_PW must be set in the environment to provide rabbit user name and password\n" )
		fmt.Fprintf( os.Stderr, "Valid arguments: add, delete, show, mirror, verbose\n" )

		rc := 0
		if ! *wants_help {
			rc = 1
		}
		
		os.Exit( rc )
	}

	if *vlevel <= 0 && *verbose {
		*vlevel = 1
	}
	sheep := bleater.Mk_bleater( *vlevel, os.Stderr )		// the master bleater should we ever have a daemon like control interface to affect all thread levels
	sheep.Set_prefix( "main" )
	sheep.Set_level( *vlevel )
	resp_key = gen_key()									// the key used as the rmq response exchange key

	req := ""
	switch( argv[0] ) {
		case "add":
			req = mk_add( argv )

		case "del", "delete":
			req = mk_del( argv )

		case "dump":
			req = mk_dump( argv )

		case "mirror":
			req = mk_mirror( argv )

		case "ping":				// ping passed to VFd for response
			req = mk_ping( argv )

		case "Ping":				// ping answered by tokay
			req = mk_ping( argv )
			sheep.Baa( 1, "sending request: %s", req )

		case "show":
			req = mk_show( argv )

		case "verbose":

		default:
			sheep.Baa( 0, "unrecognised action type: %s", argv[0] )
			flag.Usage()
			os.Exit( 1 )

	}

	if req == "" {
		sheep.Baa( 0, "invalid arguments for %s", argv[1] )
		flag.Usage()
		os.Exit( 1 )
	}
	sheep.Baa( 2, "sending req: %s", req )
	
	pw = os.Getenv( "RMQ_PW" )				// user name and password must come from environment so it's not exposed on the command line
	uname = os.Getenv( "RMQ_UNAME" )

	if pw == "" || uname == ""  {
		fmt.Fprintf( os.Stderr, "rabbitMQ username and password must be defined in the environment (RMQ_PW, RMQ_UNAME)\n" )
		os.Exit( 1 )
	}

	etype := "direct+!du+ad"								// direct, auto delete, not durable
	ekey := "tokay_req"										// default key for request messages

	tokens := strings.Split( *exchange, ":" )				// exch might be exname:etype:key where type might be <type>+[!]<attr1>[!]<attr2>.. etc
	switch len( tokens ) {
		case 1:	
			break;					// just exchange name

		case 3:						// exchagne etype and key
			ekey = tokens[2]
			fallthrough
		case 2:						// exchange etype
			exchange = &tokens[0]
			etype = tokens[1]
	}

	sheep.Baa( 1, "attaching writer to %s@%s:%s ex=%s etype=%s wkey=%s", uname, *ex_host, *rmqport, *exchange, etype, ekey )
	w, err := rabbit_hole.Mk_mqwriter( *ex_host, "5672", uname, pw, *exchange, etype, &ekey )		// attach to the exchange for writing
	if err != nil {
		sheep.Baa(  0, "abort: unable to attach a writer to %s: %s\n", exchange, err )
		os.Exit( 1 )
	}
	defer w.Close( )
	w.Start_writer( ekey )				// let it loose

	ekey = resp_key										// shouldn't be overriden below, but allow for testing maybe?
	etype = "direct+!du+ad"								// can be supplied on exchange name, but we hope it's not needed

	tokens = strings.Split( *rexch, ":" )				// prep response reader exchange
	switch len( tokens ) {
		case 1:	
			break;					// just exchange name everything unchanged

		case 3:						// exchagne etype and key
			//ekey = tokens[2]		// we don't allow response key to be fiddled since we already generated the request before setting connections
			fallthrough
		case 2:						// exchange etype
			rexch = &tokens[0]
			etype = tokens[1]
	}
	r, err := rabbit_hole.Mk_mqreader( *ex_host, *rmqport, uname, pw, *rexch, etype, &ekey )		// collector expected to close r on return
	sheep.Baa( 1, "attaching reader to %s@%s:%s ex=%s etype=%s rkey=%s", uname, *ex_host, *rmqport, *rexch, etype, ekey )
	if err != nil {
		fmt.Fprintf( os.Stderr, "abort: unable to attach a reader on %s: %s\n", rexch, err )
		os.Exit( 1 )
	}

	go collector( *rexch, r, 1, *raw_json, &wg, sheep )			// wait for the one message we expect, write to stdout and stop
	wg.Add( 1 )													// up the number of collectors we are waiting on
	sheep.Baa( 1, "bidirectional communication established" )

	w.Port <- req						// send the request, then hang tight until collector hears back
	wg.Wait()		// wait for the collector to finish
}
