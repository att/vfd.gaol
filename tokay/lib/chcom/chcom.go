// vi: sw=4 ts=4:
/*
	Mnemonic:	chcom.go
	Abstract:	Channel communications interfaces specific to package here
				(more specific than gopkgs/ipc provides, and not generic
				enough to port up into that library).

				Right now these just define the structs passed, but might someday
				define operations on them.

	Date:		16 March 2018
	Author:		E. Scott Daniels
*/

package chcom

import (
	"github.com/att/gopkgs/jsontools"
)


/*
	Request passed to the serialiser for forwarding to downstream processes.
*/
type Request struct {
	Rid		string						// request id sent to VFd that we expect in its response
	Exch_key string						// the key that requestor expects it's messages to have on the exchange
	Msg_key	string						// message key that user provides allowing it to disabmiguate responses (we ignore, just pass back)
	Source	string						// may determine the type of data put on writer channel
	Jtree	*jsontools.Jtree			// cracked json from request
	Resp_ch	chan interface{}			// channel for a response
	Single_use bool;					// set to true if this is a single use channel and writer should close
}

/*
	A reponse block passed to the responder when it needs to return a response, 
	possibly after waiting for it to come back from the downstream.
*/
type Response struct {					// info that we will cache while waiting on a response
	Rid		string						// our rid pulled from request for easier access
	Exch_key	string					// pulled from the request for easier access
	Msg_key	string						// pulled from request for easier access
	Tstamp	int64						// timestamp to know when the request has timed out
	Wait bool							// set to true if a response from VFd must be waited for and matched
	Req	*Request
	Rdata string						// data that came back from VFd, or error data we sent
}
