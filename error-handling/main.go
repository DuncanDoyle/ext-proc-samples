// based on https://github.com/salrashid123/envoy_ext_proc
/*
 * Other ext-proc examples:
 * - https://github.com/GoogleCloudPlatform/envoy-processor-examples/blob/main/README.md
 * - https://github.com/ekkinox/ext-proc-demo
 *
 */ 


package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	"slices"

	core_v3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	service_ext_proc_v3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/solo-io/go-utils/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

var (
	grpcport = flag.String("grpcport", ":18080", "grpcport")
)

type Instructions struct {
	// Header key/value pairs to add to the request or response.
	AddHeaders map[string]string `json:"addHeaders"`
	// Header keys to remove from the request or response.
	RemoveHeaders []string `json:"removeHeaders"`
	// Set the body of the request or response to the specified string. If empty, will be ignored.
	SetBody string `json:"setBody"`
	// Set the request or response trailers.
	SetTrailers map[string]string `json:"setTrailers"`
}

type server struct{}

type healthServer struct{}

func (s *healthServer) Check(ctx context.Context, in *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	log.Printf("Handling grpc Check request: + %s", in.String())
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

func (s *healthServer) Watch(in *grpc_health_v1.HealthCheckRequest, srv grpc_health_v1.Health_WatchServer) error {
	return status.Error(codes.Unimplemented, "Watch is not implemented")
}

func (s *server) Process(srv service_ext_proc_v3.ExternalProcessor_ProcessServer) error {
	log.Printf("Process")

	//ID of the request we're processing.
	requestId := string("")
	upstreamStatus := string("")


	//TODO Make this a constant.
	errorCodes := [] string {"401", "402", "403", "404"}


	ctx := srv.Context()
	//ddoyle: Loops until a return statement is encountered, which is basically when either an error condition is met, or the stream is closed.
	for {
		
		select {
		case <-ctx.Done():
			log.Printf("context done")
			return ctx.Err()
		default:
		}

		req, err := srv.Recv()
		if err == io.EOF {
			// envoy has closed the stream. Don't return anything and close this stream entirely
			return nil
		}
		if err != nil {
			return status.Errorf(codes.Unknown, "cannot receive stream request: %v", err)
		}

		// build response based on request type
		resp := &service_ext_proc_v3.ProcessingResponse{}
		switch v := req.Request.(type) {

		//------------------------- Request Headers -------------------------

		/*
		 * Adds the 'x-request-message-id' header to the request based on the value of 'x-request-id' header.
		 */
		case *service_ext_proc_v3.ProcessingRequest_RequestHeaders:
			log.Printf("Got RequestHeaders")

			h := req.Request.(*service_ext_proc_v3.ProcessingRequest_RequestHeaders)
			
			requestId = getHeader(h.RequestHeaders.Headers, "x-request-id")
			if requestId != "" {
				log.Printf("Request ID value: %v", requestId)
			}

			//Create the slice of headers that we want to add to the request.
			headersResponse := &service_ext_proc_v3.HeadersResponse{}
			if requestId != "" {
				//Add a new header to the request going upstream
				var addHeaders []*core_v3.HeaderValueOption
				addHeaders = append(addHeaders, 
					&core_v3.HeaderValueOption{ 
						Header: &core_v3.HeaderValue{Key: "x-request-message-id", Value: requestId},
					},
				)

				headersResponse.Response = &service_ext_proc_v3.CommonResponse{
					HeaderMutation: &service_ext_proc_v3.HeaderMutation{
						SetHeaders: addHeaders,
					},
				}
			}

			//Set the RequestHeaders field on the ProcessingResponse.
			resp = &service_ext_proc_v3.ProcessingResponse{
				Response: &service_ext_proc_v3.ProcessingResponse_RequestHeaders{
					RequestHeaders: headersResponse,
				},
			}

		// ------------------------- Request Body -------------------------

		case *service_ext_proc_v3.ProcessingRequest_RequestBody:
			log.Printf("Got RequestBody (not currently handled)")

		//------------------------- Request Trailers -------------------------

		case *service_ext_proc_v3.ProcessingRequest_RequestTrailers:
			log.Printf("Got RequestTrailers (not currently handled)")

		//------------------------- Response Headers -------------------------

		/*
		 * Set 'x-reponse-message-id' to value of 'x-request-id'
		 * Set 'x-upstream-status' to the current status of the upstream response.
		 * Check the upstream status to see if we need to change it and provide a generic error message.
		 */
		case *service_ext_proc_v3.ProcessingRequest_ResponseHeaders:
			log.Printf("Got ResponseHeaders")

			h := req.Request.(*service_ext_proc_v3.ProcessingRequest_ResponseHeaders)

			headersResponse := &service_ext_proc_v3.HeadersResponse{}

			var addHeaders []*core_v3.HeaderValueOption
			// Set the 'x-response-message-id.
			if requestId != "" {
				//Add a new header to the request going upstream
				addHeaders = append(addHeaders, 
					&core_v3.HeaderValueOption{ 
						Header: &core_v3.HeaderValue{Key: "x-response-message-id", Value: requestId},
					},
				)
			}

			// Set the upstream status code.
			// Get the ':status' pseudo header.
			upstreamStatus = getHeader(h.ResponseHeaders.GetHeaders(), ":status")

			//Add the upstreamstatus to the headers.
			addHeaders = append(addHeaders, 
				&core_v3.HeaderValueOption{
					Header: &core_v3.HeaderValue{Key: "x-upstream-status", Value: upstreamStatus},
				},
			)

			bodyMutation := &service_ext_proc_v3.BodyMutation{}
			//We want to set the standardized error message here instead of when handling the ReponseBody, as the response actually might not have a body.
			if (slices.Contains(errorCodes, upstreamStatus)) {	
				log.Printf("Building custom error response.")

				//Set the status-code for all of the errors we're processing to 400.
				addHeaders = append(addHeaders, 
					&core_v3.HeaderValueOption{ 
						Header: &core_v3.HeaderValue{Key: ":status", Value: "400"},
					},
				)

				bytesToSend := []byte(`{ "error": { "code": "whatever text we need to extract", "errodId": ` + requestId + `}}`)
				bodyMutation.Mutation = &service_ext_proc_v3.BodyMutation_Body{
						Body: bytesToSend,
				}
			}
		
			//Set the HeadersResponse.
			headersResponse.Response = &service_ext_proc_v3.CommonResponse{
				HeaderMutation: &service_ext_proc_v3.HeaderMutation{
					SetHeaders: addHeaders,
					RemoveHeaders: []string{"content-length", "x-envoy-upstream-service-time"},
				},
				BodyMutation: bodyMutation,
			}

			//Set the HeadersResponse on the processing response.
			resp = &service_ext_proc_v3.ProcessingResponse{
				Response: &service_ext_proc_v3.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: headersResponse,
				},
			}

		//------------------------- Response Body -------------------------

		//Seems this is not called when the upstream response does not have a body.
		case *service_ext_proc_v3.ProcessingRequest_ResponseBody:
			log.Printf("Got ResponseBody (not currently handled)")

			b := req.Request.(*service_ext_proc_v3.ProcessingRequest_ResponseBody)
			log.Printf("ResponseBody.Body: %s", string(b.ResponseBody.Body))
			log.Printf("ResponseBody.EndOfStream: %T", b.ResponseBody.EndOfStream)

			if b.ResponseBody.EndOfStream {
				if (slices.Contains(errorCodes, upstreamStatus)) {
					bytesToSend := []byte(`{ "error": { "code": "whatever text we need to extract", "errodId": ` + requestId + `}}`)
					resp = &service_ext_proc_v3.ProcessingResponse{
						Response: &service_ext_proc_v3.ProcessingResponse_ResponseBody{
							ResponseBody: &service_ext_proc_v3.BodyResponse{
								Response: &service_ext_proc_v3.CommonResponse{
									BodyMutation: &service_ext_proc_v3.BodyMutation{
										Mutation: &service_ext_proc_v3.BodyMutation_Body{
											Body: bytesToSend,
										},
									},
								},
							},
						},
					}
				}
			}

		//------------------------- Response Trailers -------------------------

		case *service_ext_proc_v3.ProcessingRequest_ResponseTrailers:
			log.Printf("Got ResponseTrailers (not currently handled)")

		default:
			log.Printf("Unknown Request type %v", v)
		}

		// At this point we believe we have created a valid response...
		// note that this is sometimes not the case
		// anyways for now just send it
		log.Printf("Sending ProcessingResponse")
		if err := srv.Send(resp); err != nil {
			log.Printf("send error %v", err)
			return err
		}

	}
}

/**
 * Returns the header value for a given header key.
 * If the header does not exist, we return an empty string.
 */
func getHeader(headers *core_v3.HeaderMap, key string) string {
	//Interestingly enough .... this "map" does not provide a lookup function based on the key .....
	//.... So I guess we need to traverse the entire thing to find the header value we're after .....
	for _, header := range headers.Headers {
		if header.GetKey() == key {	
			return header.GetValue()
		} 
	}
	return ""
}

/*
 * Main function which starts the gRPC server.
 */
func main() {

	flag.Parse()

	lis, err := net.Listen("tcp", *grpcport)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	sopts := []grpc.ServerOption{grpc.MaxConcurrentStreams(1000)}
	s := grpc.NewServer(sopts...)

	service_ext_proc_v3.RegisterExternalProcessorServer(s, &server{})

	grpc_health_v1.RegisterHealthServer(s, &healthServer{})

	log.Printf("Starting gRPC server on port %s", *grpcport)

	var gracefulStop = make(chan os.Signal, 1)
	signal.Notify(gracefulStop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-gracefulStop
		log.Printf("caught sig: %+v", sig)
		time.Sleep(time.Second)
		log.Printf("Graceful stop completed")
		os.Exit(0)
	}()
	err = s.Serve(lis)
	if err != nil {
		log.Fatalf("killing server with %v", err)
	}
}

func getInstructionsFromHeaders(in *service_ext_proc_v3.HttpHeaders) string {
	for _, n := range in.Headers.Headers {
		if n.Key == "instructions" {
			return n.Value
		}
	}
	return ""
}

func getHeadersResponseFromInstructions(in *service_ext_proc_v3.HttpHeaders) (*service_ext_proc_v3.HeadersResponse, error) {
	instructionString := getInstructionsFromHeaders(in)

	// no instructions were sent, so don't modify anything
	if instructionString == "" {
		return &service_ext_proc_v3.HeadersResponse{}, nil
	}

	var instructions *Instructions
	err := json.Unmarshal([]byte(instructionString), &instructions)
	if err != nil {
		log.Printf("Error unmarshalling instructions: %v", err)
		return nil, err
	}

	// build the response
	resp := &service_ext_proc_v3.HeadersResponse{
		Response: &service_ext_proc_v3.CommonResponse{},
	}

	// headers
	if len(instructions.AddHeaders) > 0 || len(instructions.RemoveHeaders) > 0 {
		var addHeaders []*core_v3.HeaderValueOption
		for k, v := range instructions.AddHeaders {
			addHeaders = append(addHeaders, &core_v3.HeaderValueOption{
				Header: &core_v3.HeaderValue{Key: k, Value: v},
			})
		}
		resp.Response.HeaderMutation = &service_ext_proc_v3.HeaderMutation{
			SetHeaders:    addHeaders,
			RemoveHeaders: instructions.RemoveHeaders,
		}
	}

	// body
	if instructions.SetBody != "" {
		body := []byte(instructions.SetBody)

		if resp.Response.HeaderMutation == nil {
			resp.Response.HeaderMutation = &service_ext_proc_v3.HeaderMutation{}
		}
		resp.Response.HeaderMutation.SetHeaders = append(resp.Response.HeaderMutation.SetHeaders,
			[]*core_v3.HeaderValueOption{
				{
					Header: &core_v3.HeaderValue{
						Key:   "content-type",
						Value: "text/plain",
					},
				},
				{
					Header: &core_v3.HeaderValue{
						Key:   "Content-Length",
						Value: strconv.Itoa(len(body)),
					},
				},
			}...)
		resp.Response.BodyMutation = &service_ext_proc_v3.BodyMutation{
			Mutation: &service_ext_proc_v3.BodyMutation_Body{
				Body: body,
			},
		}
	}

	// trailers
	if len(instructions.SetTrailers) > 0 {
		var setTrailers []*core_v3.HeaderValue
		for k, v := range instructions.SetTrailers {
			setTrailers = append(setTrailers, &core_v3.HeaderValue{Key: k, Value: v})
		}
		resp.Response.Trailers = &core_v3.HeaderMap{
			Headers: setTrailers,
		}
	}

	return resp, nil
}
