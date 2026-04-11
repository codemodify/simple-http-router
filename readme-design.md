# design considerations
- keep config dead simple to start with, IO proof and logical
- keep config powerful, expand as you need, everything is configurable where it makes sense
- no third party dependency, Go stadard library is sufficient, check the [go.mod](./go.mod)
- when a feature is about bloat (ex: metrics, load balancing, admin API, etc) then move it out, let something else handle it
- "keep it simple but dangerous" basically

# additional design considerations (to be updated)
- personal intent is to make this part of an eco-system, but fully usable independently
- imagine this: INTERNET hits `simple-http-router`
	- then routed to `simple-http-security` for IP allow/block lists
	- then routed to `simple-http-balancer` for load balancing
		- OR routed to `HAProxy` for load balancing
