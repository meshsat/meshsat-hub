package takoperator

import "strings"

// The instance's internal admin gateway.
//
// OpenTAKServer's REST API listens on 127.0.0.1:8081 inside the pod and nothing
// outside it can reach it, which is how we want it: that API can create users,
// change groups and read configuration. But the Hub does need a narrow slice of
// it, to create and deactivate the TAK users a customer adds. So one nginx sits
// in the pod and exposes exactly that slice on 8444, to exactly one client.
//
// Three properties matter more than anything else here, and each has a test or a
// release-gate assertion behind it:
//
//  1. Only CN=meshsat-hub gets in. The listener verifies client certificates
//     against the tenant's own CA, and then checks the common name as well —
//     because that CA also signs every phone in the tenant, and a phone
//     certificate must not be an admin credential.
//  2. Only /api/user/ and /api/groups are proxied. Everything else is 404,
//     including /api/config, which would hand over the instance's secrets.
//  3. X-Ssl-Cert is always overwritten, never passed through. OpenTAKServer
//     trusts that header to identify the connecting EUD, so a client that could
//     set it could claim to be any phone.
func nginxConfig() string {
	return strings.TrimLeft(`
# Rendered by the tak-operator. Do not edit in the pod: it is replaced on every
# reconcile.
worker_processes 1;
error_log /dev/stderr warn;
pid /tmp/nginx.pid;

events {
    worker_connections 256;
}

http {
    access_log off;
    client_body_temp_path /tmp/client_body;
    proxy_temp_path /tmp/proxy;
    fastcgi_temp_path /tmp/fastcgi;
    uwsgi_temp_path /tmp/uwsgi;
    scgi_temp_path /tmp/scgi;

    # A request body large enough for a group membership change and no larger.
    client_max_body_size 64k;
    proxy_read_timeout 30s;

    # The DN arrives in RFC 2253 form, so the common name is one component of a
    # comma-separated string: CN=meshsat-hub,O=MeshSat. Matching the component
    # rather than a substring keeps a user called "meshsat-hubbish" out.
    map $ssl_client_s_dn $is_hub {
        default                     0;
        "~(^|,)CN=meshsat-hub(,|$)" 1;
    }

    server {
        # Plain HTTP is not offered at all: there is no listener without mutual
        # TLS, so a misrouted request cannot reach the API unauthenticated.
        listen 8444 ssl;
        http2 on;

        ssl_certificate     /etc/tak/tls/tls.crt;
        ssl_certificate_key /etc/tak/tls/tls.key;
        ssl_client_certificate /etc/tak/ca/ca.pem;
        ssl_verify_client on;
        ssl_protocols TLSv1.2 TLSv1.3;

        # Never inherited from the request. OpenTAKServer reads this header to
        # decide which EUD it is talking to.
        proxy_set_header X-Ssl-Cert "";

        # Default deny. Anything not named below does not exist as far as a
        # caller is concerned, which includes /api/config, /api/truststore,
        # /Marti, /oauth, /socket.io and the web UI.
        location / {
            return 404;
        }

        location /api/user/ {
            if ($is_hub = 0) { return 403; }
            proxy_pass http://127.0.0.1:8081;
            proxy_set_header Host $host;
        }

        # Exact-prefix, because /api/groups and /api/groups/<name> are both used
        # and /api/groupsomething is not a thing we want to forward by accident.
        location ^~ /api/groups {
            if ($is_hub = 0) { return 403; }
            proxy_pass http://127.0.0.1:8081;
            proxy_set_header Host $host;
        }

        # A liveness target that needs no client certificate would need its own
        # listener; instead the pod's probes are TCP, and this answers for a
        # human with the right certificate.
        location = /healthz {
            if ($is_hub = 0) { return 403; }
            return 200 "ok\n";
        }
    }
}
`, "\n")
}
