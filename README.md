A simple authoritative DNS server to use in Kubernetes. Example configuration:

```json
{
  "fqdn": "example.my-domain.com",
  "key": "my-domain",
  "defaultService": "kube-system/traefik"
}
```

On a service:
```yaml
spec:
  name: test
  annotations:
    ns/key: my-domain
```

Now, querying the server for `test.example.my-domain.com` will return the IP address of the service (if it is a LoadBalancer) or the IP address of the default service (which must be a LoadBalancer).