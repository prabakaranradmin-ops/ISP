# ISP BSS walled garden for soft-suspended subscribers (FR-BIL-004)
#
# What it does
#   RADIUS puts every soft-suspended session on the address list
#   "isp-suspended" (the Mikrotik-Address-List attribute). These rules let
#   that list reach DNS, the subscriber portal and the payment gateway, send
#   its plain-HTTP web traffic to the payment-page redirector, and refuse
#   everything else. Without these rules on a router, soft suspension on
#   that router restricts nothing: the subscriber is on the list and the
#   list matches no rule.
#
#   Do not rename "isp-suspended". RADIUS sends that exact name
#   (internal/nas/walledgarden.go), and a test fails if this file names any
#   other.
#
# Before running it
#   1. On the API server, set in the environment and restart the API:
#        WALLED_GARDEN_ADDR=:8088
#        WALLED_GARDEN_REDIRECT_URL=https://<portal host>/ui/login?suspended=1
#      and allow inbound TCP 8088 from the routers in the server's firewall.
#   2. Set the four values just below.
#
# Run it with
#   /import file-name=walled-garden.rsc
# Running it again replaces what an earlier run added. It removes only rules
# carrying the comment "isp-bss walled garden" and entries on the
# "isp-walled-garden" list, so it never touches the operator's own rules.
#
# Limits worth knowing
#   - IPv4 only. Mikrotik-Address-List lists the session's IPv4 address.
#     RADIUS withholds a plan's Mikrotik-Delegated-IPv6-Pool from a walled
#     session, but IPv6 handed out by the PPP profile itself is not covered.
#     Add matching /ipv6 firewall rules if subscribers get IPv6 that way.
#   - HTTPS cannot be redirected without a certificate warning, so HTTPS to
#     anything outside the walled garden is refused (TCP reset) rather than
#     redirected. Phones and laptops notice from their plain-HTTP
#     connectivity check and show their own "sign in to network" prompt,
#     which lands on the payment page. A subscriber typing an address gets
#     the page only if the browser tries plain HTTP.
#   - Payment methods that leave the gateway's own pages (net banking, some
#     card 3-D Secure pages) need their hosts added to gatewayHosts, or the
#     subscriber pays over mobile data. So does a UPI app paying from the
#     same Wi-Fi.
#   - The gateway hosts below are Razorpay's as this was written. Check them
#     against Razorpay's current documentation before relying on them.
#
# Written against RouterOS v6.45 and v7 syntax. The repository's tests check
# only that this file names the list RADIUS sends; it has not been run
# against a router from here. Try it on a lab router first.

{
:local portalHost "portal.example.net"
:local landingIP "192.0.2.10"
:local landingPort 8088
:local gatewayHosts {"rzp.io";"razorpay.com";"api.razorpay.com";"checkout.razorpay.com";"cdn.razorpay.com";"lumberjack.razorpay.com"}

:local tag "isp-bss walled garden"

# Start clean: only what an earlier run of this script added.
/ip firewall filter remove [find where comment~"isp-bss walled garden"]
/ip firewall nat remove [find where comment~"isp-bss walled garden"]
/ip firewall address-list remove [find where list="isp-walled-garden"]

# Where a suspended subscriber may still go. Host names are resolved by the
# router and kept up to date as their addresses change.
/ip firewall address-list add list=isp-walled-garden address=$portalHost comment="$tag: subscriber portal"
/ip firewall address-list add list=isp-walled-garden address=$landingIP comment="$tag: payment-page redirector"
:foreach h in=$gatewayHosts do={
  /ip firewall address-list add list=isp-walled-garden address=$h comment="$tag: payment gateway"
}

# Plain-HTTP web traffic to anywhere outside the garden goes to the
# redirector, which answers every request with the payment page. Placed ahead
# of every static NAT rule so nothing earlier sends it elsewhere.
:local natFirst [/ip firewall nat find where dynamic=no]
:if ([:len $natFirst] > 0) do={
  /ip firewall nat add chain=dstnat src-address-list=isp-suspended dst-address-list=!isp-walled-garden protocol=tcp dst-port=80 action=dst-nat to-addresses=$landingIP to-ports=$landingPort comment="$tag: web traffic to the payment page" place-before=($natFirst->0)
} else={
  /ip firewall nat add chain=dstnat src-address-list=isp-suspended dst-address-list=!isp-walled-garden protocol=tcp dst-port=80 action=dst-nat to-addresses=$landingIP to-ports=$landingPort comment="$tag: web traffic to the payment page"
}

# Everything a suspended subscriber sends through the router is sent to its
# own chain first, ahead of every static filter rule, including any
# fasttrack or accept-established rule that would otherwise let it pass.
:local filterFirst [/ip firewall filter find where dynamic=no]
:if ([:len $filterFirst] > 0) do={
  /ip firewall filter add chain=forward src-address-list=isp-suspended action=jump jump-target=isp-walled comment="$tag: suspended subscribers" place-before=($filterFirst->0)
} else={
  /ip firewall filter add chain=forward src-address-list=isp-suspended action=jump jump-target=isp-walled comment="$tag: suspended subscribers"
}

# The chain itself: DNS, then the garden (the redirector included, which is
# where the NAT rule above sends web traffic), then nothing.
/ip firewall filter add chain=isp-walled protocol=udp dst-port=53 action=accept comment="$tag: DNS"
/ip firewall filter add chain=isp-walled protocol=tcp dst-port=53 action=accept comment="$tag: DNS"
/ip firewall filter add chain=isp-walled dst-address-list=isp-walled-garden action=accept comment="$tag: portal, payment gateway, redirector"
/ip firewall filter add chain=isp-walled protocol=tcp action=reject reject-with=tcp-reset comment="$tag: everything else"
/ip firewall filter add chain=isp-walled action=reject reject-with=icmp-admin-prohibited comment="$tag: everything else"

:put "isp-bss walled garden installed. Soft-suspended sessions (address list isp-suspended) now reach only DNS, $portalHost, the payment gateway and the redirector at $landingIP:$landingPort."
}
