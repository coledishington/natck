ip netns add staging
ip netns add client
ip netns add middleware
ip netns add server

ip -n staging link add veth0 type veth peer name veth1
ip -n staging link set veth0 netns middleware
ip -n staging link set veth1 netns client
ip -n client link set veth1 name veth0

ip -n staging link add veth0 type veth peer name veth1
ip -n staging link set veth1 netns middleware
ip -n staging link set veth0 netns server

ip netns del staging

ip -n client addr add 192.168.64.2/24 dev veth0
ip -n middleware addr add 192.168.64.1/24 dev veth0
ip -n middleware addr add 192.168.65.1/24 dev veth1
ip -n server addr add 192.168.65.2/24 dev veth0

ip -n client link set veth0 up 
ip -n middleware link set veth0 up
ip -n middleware link set veth1 up
ip -n server link set veth0 up

# ip netns exec client ping 192.168.65.2

ip -n client route add 0.0.0.0/0 via 192.168.64.1
ip -n server route add 0.0.0.0/0 via 192.168.65.1

ip netns exec middleware sysctl net.ipv4.ip_forward=1
ip netns exec middleware iptables -t nat -A POSTROUTING -p TCP -j MASQUERADE --to-ports 40000-40002

mkdir /tmp/net-ck

cat <<EOF > /tmp/net-ck/index.html
<!doctype html>
<html lang="en-US">
  <head>
    <meta charset="utf-8" />
    <title>My index page</title>
  </head>
  <body>
    <p>My index page</p>
    <a href='/url1.html'>This new url test</a>
  </body>
</html>
EOF

cat <<EOF > /tmp/net-ck/url1.html
<!doctype html>
<html lang="en-US">
  <head>
    <meta charset="utf-8" />
    <title>My test ur1 page</title>
  </head>
  <body>
    <p>This is my test ur1 page</p>
  </body>
</html>
EOF

( cd /tmp/net-ck && ip netns exec server python3 -m http.server 8080 & )
ip netns exec server ss -tlpn

ip netns exec client curl http://192.168.65.2:8080/index.html
ip netns exec client ./cgnatck
