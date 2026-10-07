# fail2ban

`filter.d/mountenant.conf` matches failed logins and invalid download links in
Mountenant's security log (specification §7.8); `jail.d/mountenant.conf` is an
example jail. `sample.log` is generated from the logger's own output by
`internal/platform/logging/fail2ban_test.go`, and CI checks it with
`fail2ban-regex`:

```sh
fail2ban-regex deploy/fail2ban/sample.log deploy/fail2ban/filter.d/mountenant.conf
```

The logged IP is the client address resolved through `server.trustedProxies`,
never the proxy's own address.
