# NGX Storage management API TLS

The iSCSI driver connects to the NGX Storage management API over HTTPS. The current driver configures `InsecureSkipVerify: true`; it does not validate the server certificate chain or hostname and has no custom CA Secret option. This setting applies to the management API client. It must not be described as certificate-validated TLS.

CSI gRPC communication between Kubernetes components and the driver uses configured Unix sockets.
