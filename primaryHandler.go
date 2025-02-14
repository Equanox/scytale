/**
 * Copyright 2019 Comcast Cable Communications Management, LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	gokithttp "github.com/go-kit/kit/transport/http"
	"github.com/goph/emperror"
	"github.com/gorilla/mux"
	"github.com/justinas/alice"
	"github.com/spf13/viper"
	"github.com/xmidt-org/bascule"
	"github.com/xmidt-org/bascule/basculehttp"
	"github.com/xmidt-org/webpa-common/basculechecks"
	"github.com/xmidt-org/webpa-common/logging"
	"github.com/xmidt-org/webpa-common/logging/logginghttp"
	"github.com/xmidt-org/webpa-common/service"
	"github.com/xmidt-org/webpa-common/service/monitor"
	"github.com/xmidt-org/webpa-common/xhttp"
	"github.com/xmidt-org/webpa-common/xhttp/fanout"
	"github.com/xmidt-org/webpa-common/xmetrics"
	"github.com/xmidt-org/wrp-go/wrp"
	"github.com/xmidt-org/wrp-go/wrp/wrphttp"
)

const (
	baseURI = "/api"
	version = "v2"
)

func SetLogger(logger log.Logger) func(delegate http.Handler) http.Handler {
	return func(delegate http.Handler) http.Handler {
		return http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				ctx := r.WithContext(logging.WithLogger(r.Context(),
					log.With(logger, "requestHeaders", r.Header, "requestURL", r.URL.EscapedPath(), "method", r.Method)))
				delegate.ServeHTTP(w, ctx)
			})
	}
}

func GetLogger(ctx context.Context) bascule.Logger {
	logger := log.With(logging.GetLogger(ctx), "ts", log.DefaultTimestampUTC, "caller", log.DefaultCaller)
	return logger
}

func populateMessage(ctx context.Context, message *wrp.Message) {
	if auth, ok := bascule.FromContext(ctx); ok {
		if token := auth.Token; token != nil {
			if ids, ok := token.Attributes().Get("partnerIDs"); ok {
				if idStr, ok := ids.([]string); ok {
					message.PartnerIDs = idStr
				}
			}
		}
	}
}

func authChain(v *viper.Viper, logger log.Logger, registry xmetrics.Registry) (alice.Chain, error) {
	var (
		m *basculechecks.JWTValidationMeasures
	)

	if registry != nil {
		m = basculechecks.NewJWTValidationMeasures(registry)
	}
	listener := basculechecks.NewMetricListener(m)

	basicAllowed := make(map[string]string)
	basicAuth := v.GetStringSlice("authHeader")
	for _, a := range basicAuth {
		decoded, err := base64.StdEncoding.DecodeString(a)
		if err != nil {
			logging.Info(logger).Log(logging.MessageKey(), "failed to decode auth header", "authHeader", a, logging.ErrorKey(), err.Error())
		}

		i := bytes.IndexByte(decoded, ':')
		logging.Debug(logger).Log(logging.MessageKey(), "decoded string", "string", decoded, "i", i)
		if i > 0 {
			basicAllowed[string(decoded[:i])] = string(decoded[i+1:])
		}
	}
	logging.Debug(logger).Log(logging.MessageKey(), "Created list of allowed basic auths", "allowed", basicAllowed, "config", basicAuth)

	options := []basculehttp.COption{basculehttp.WithCLogger(GetLogger), basculehttp.WithCErrorResponseFunc(listener.OnErrorResponse)}
	if len(basicAllowed) > 0 {
		options = append(options, basculehttp.WithTokenFactory("Basic", basculehttp.BasicTokenFactory(basicAllowed)))
	}
	var jwtVal JWTValidator

	v.UnmarshalKey("jwtValidator", &jwtVal)
	if jwtVal.Keys.URI != "" {
		resolver, err := jwtVal.Keys.NewResolver()
		if err != nil {
			return alice.Chain{}, emperror.With(err, "failed to create resolver")
		}

		options = append(options, basculehttp.WithTokenFactory("Bearer", basculehttp.BearerTokenFactory{
			DefaultKeyId: DefaultKeyID,
			Resolver:     resolver,
			Parser:       bascule.DefaultJWTParser,
			Leeway:       jwtVal.Leeway,
		}))
	}

	authConstructor := basculehttp.NewConstructor(options...)

	bearerRules := bascule.Validators{
		bascule.CreateNonEmptyPrincipalCheck(),
		bascule.CreateNonEmptyTypeCheck(),
		bascule.CreateValidTypeCheck([]string{"jwt"}),
	}

	// only add capability check if the configuration is set
	var capabilityConfig basculechecks.CapabilityConfig
	v.UnmarshalKey("capabilityConfig", &capabilityConfig)
	if capabilityConfig.FirstPiece != "" && capabilityConfig.SecondPiece != "" && capabilityConfig.ThirdPiece != "" {
		bearerRules = append(bearerRules, bascule.CreateListAttributeCheck("capabilities", basculechecks.CreateValidCapabilityCheck(capabilityConfig)))
	}

	authEnforcer := basculehttp.NewEnforcer(
		basculehttp.WithELogger(GetLogger),
		basculehttp.WithRules("Basic", bascule.Validators{
			bascule.CreateAllowAllCheck(),
		}),
		basculehttp.WithRules("Bearer", bearerRules),
		basculehttp.WithEErrorResponseFunc(listener.OnErrorResponse),
	)

	return alice.New(SetLogger(logger), authConstructor, authEnforcer, basculehttp.NewListenerDecorator(listener)), nil
}

// createEndpoints examines the configuration and produces an appropriate fanout.Endpoints, either using the configured
// endpoints or service discovery.
func createEndpoints(logger log.Logger, cfg fanout.Configuration, registry xmetrics.Registry, e service.Environment) (fanout.Endpoints, error) {

	if len(cfg.Endpoints) > 0 {
		logger.Log(level.Key(), level.InfoValue(), logging.MessageKey(), "using configured endpoints for fanout", "endpoints", cfg.Endpoints)
		return fanout.ParseURLs(cfg.Endpoints...)
	} else if e != nil {
		logger.Log(level.Key(), level.InfoValue(), logging.MessageKey(), "using service discovery for fanout")
		endpoints := fanout.NewServiceEndpoints(fanout.WithAccessorFactory(e.AccessorFactory()))

		_, err := monitor.New(
			monitor.WithLogger(logger),
			monitor.WithFilter(monitor.NewNormalizeFilter(e.DefaultScheme())),
			monitor.WithEnvironment(e),
			monitor.WithListeners(
				monitor.NewMetricsListener(registry),
				endpoints,
			),
		)

		return endpoints, err
	}

	return nil, errors.New("Unable to create endpoints")
}

func NewPrimaryHandler(logger log.Logger, v *viper.Viper, registry xmetrics.Registry, e service.Environment) (http.Handler, error) {
	var cfg fanout.Configuration
	if err := v.UnmarshalKey("fanout", &cfg); err != nil {
		return nil, err
	}
	logging.Error(logger).Log(logging.MessageKey(), "creating primary handler")

	endpoints, err := createEndpoints(logger, cfg, registry, e)
	if err != nil {
		return nil, err
	}

	authChain, err := authChain(v, logger, registry)
	if err != nil {
		return nil, err
	}

	var (
		handlerChain = authChain.Extend(
			fanout.NewChain(
				cfg,
				logginghttp.SetLogger(
					logger,
					logginghttp.RequestInfo,

					// custom logger func that extracts the intended destination of requests
					func(kv []interface{}, request *http.Request) []interface{} {
						if deviceName := request.Header.Get("X-Webpa-Device-Name"); len(deviceName) > 0 {
							return append(kv, "X-Webpa-Device-Name", deviceName)
						}

						if variables := mux.Vars(request); len(variables) > 0 {
							if deviceID := variables["deviceID"]; len(deviceID) > 0 {
								return append(kv, "deviceID", deviceID)
							}
						}

						return kv
					},
				),
			),
		)

		transactor = fanout.NewTransactor(cfg)
		options    = []fanout.Option{
			fanout.WithTransactor(transactor),
		}
	)

	if len(cfg.Authorization) > 0 {
		options = append(
			options,
			fanout.WithClientBefore(
				gokithttp.SetRequestHeader("Authorization", "Basic "+cfg.Authorization),
			),
		)
	}

	var (
		router        = mux.NewRouter()
		sendSubrouter = router.Path(fmt.Sprintf("%s/%s/device", baseURI, version)).Methods("POST", "PUT").Subrouter()
	)

	router.NotFoundHandler = http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusBadRequest)
	})

	sendSubrouter.Headers(wrphttp.MessageTypeHeader, "").Handler(
		handlerChain.Then(
			fanout.New(
				endpoints,
				append(
					options,
					fanout.WithFanoutBefore(
						fanout.UsePath(fmt.Sprintf("%s/%s/device/send", baseURI, version)),
						func(ctx context.Context, original, fanout *http.Request, body []byte) (context.Context, error) {
							message, err := wrphttp.NewMessageFromHeaders(original.Header, bytes.NewReader(body))
							if err != nil {
								return ctx, err
							}

							populateMessage(ctx, message)
							var buffer bytes.Buffer
							if err := wrp.NewEncoder(&buffer, wrp.Msgpack).Encode(message); err != nil {
								return ctx, err
							}

							fanoutBody := buffer.Bytes()
							fanout.Body, fanout.GetBody = xhttp.NewRewindBytes(fanoutBody)
							fanout.ContentLength = int64(len(fanoutBody))
							fanout.Header.Set("Content-Type", wrp.Msgpack.ContentType())
							fanout.Header.Set("X-Webpa-Device-Name", message.Destination)
							return ctx, nil
						},
					),
					fanout.WithFanoutFailure(
						fanout.ReturnHeadersWithPrefix("X-"),
					),
					fanout.WithFanoutAfter(
						fanout.ReturnHeadersWithPrefix("X-"),
					),
				)...,
			),
		),
	)

	sendSubrouter.Headers("Content-Type", wrp.JSON.ContentType()).Handler(
		handlerChain.Then(
			fanout.New(
				endpoints,
				append(
					options,
					fanout.WithFanoutBefore(
						fanout.UsePath(fmt.Sprintf("%s/%s/device/send", baseURI, version)),
						func(ctx context.Context, original, fanout *http.Request, body []byte) (context.Context, error) {
							var (
								message wrp.Message
								decoder = wrp.NewDecoderBytes(body, wrp.JSON)
							)

							if err := decoder.Decode(&message); err != nil {
								return ctx, err
							}

							populateMessage(ctx, &message)
							var buffer bytes.Buffer
							if err := wrp.NewEncoder(&buffer, wrp.Msgpack).Encode(&message); err != nil {
								return ctx, err
							}

							fanoutBody := buffer.Bytes()
							fanout.Body, fanout.GetBody = xhttp.NewRewindBytes(fanoutBody)
							fanout.ContentLength = int64(len(fanoutBody))
							fanout.Header.Set("Content-Type", wrp.Msgpack.ContentType())
							fanout.Header.Set("X-Webpa-Device-Name", message.Destination)
							return ctx, nil
						},
					),
					fanout.WithFanoutFailure(
						fanout.ReturnHeadersWithPrefix("X-"),
					),
					fanout.WithFanoutAfter(
						fanout.ReturnHeadersWithPrefix("X-"),
					),
				)...,
			),
		),
	)

	sendSubrouter.Headers("Content-Type", wrp.Msgpack.ContentType()).Handler(
		handlerChain.Then(
			fanout.New(
				endpoints,
				append(
					options,
					fanout.WithFanoutBefore(
						fanout.UsePath(fmt.Sprintf("%s/%s/device/send", baseURI, version)),
						func(ctx context.Context, original, fanout *http.Request, body []byte) (context.Context, error) {
							var (
								message wrp.Message
								decoder = wrp.NewDecoderBytes(body, wrp.Msgpack)
							)

							if err := decoder.Decode(&message); err != nil {
								return ctx, err
							}

							populateMessage(ctx, &message)
							var buffer bytes.Buffer
							if err := wrp.NewEncoder(&buffer, wrp.Msgpack).Encode(&message); err != nil {
								return ctx, err
							}

							fanoutBody := buffer.Bytes()
							fanout.Body, fanout.GetBody = xhttp.NewRewindBytes(fanoutBody)
							fanout.ContentLength = int64(len(fanoutBody))
							fanout.Header.Set("Content-Type", wrp.Msgpack.ContentType())
							fanout.Header.Set("X-Webpa-Device-Name", message.Destination)
							return ctx, nil
						},
					),
					fanout.WithFanoutFailure(
						fanout.ReturnHeadersWithPrefix("X-"),
					),
					fanout.WithFanoutAfter(
						fanout.ReturnHeadersWithPrefix("X-"),
					),
				)...,
			),
		),
	)

	router.Handle(
		fmt.Sprintf("%s/%s/device/{deviceID}/stat", baseURI, version),
		handlerChain.Then(
			fanout.New(
				endpoints,
				append(
					options,
					fanout.WithFanoutBefore(
						fanout.ForwardVariableAsHeader("deviceID", "X-Webpa-Device-Name"),
					),
					fanout.WithFanoutFailure(
						fanout.ReturnHeadersWithPrefix("X-"),
					),
					fanout.WithFanoutAfter(
						fanout.ReturnHeadersWithPrefix("X-"),
					),
				)...,
			),
		),
	).Methods("GET")

	return router, nil
}
