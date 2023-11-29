import csv
import graphviz
import re

CODENAMES = [
    (r"^hc-.*\.s3\..*\.amazonaws\.com$", "S3 Datalake Storage"),
    (r"^s3\.(.*\.)?amazonaws\.com$", "S3 Storage"),
    (r"^([0-9a-f]+-)+[0-9a-f]+\.(.*)hana\..*\.hanacloud\.ondemand\.com$", "HANA Cloud Instance"),
    (r"^([0-9a-f]+-)+[0-9a-z]+$", "HANA Cloud Instance"),
    (r"^([0-9a-f]+-)+[0-9a-f]+\.files\.hdl\..*\.hanacloud\.ondemand\.com$", "HDL Files Instance"),
    (r"^([0-9a-f]+-)+[0-9a-f]+\.iq\.hdl\..*\.hanacloud\.ondemand\.com$", "HDLRE Writer Instance"),
    (r"^([0-9a-f]+-)+[0-9a-f]+-coord\.iq\.hdl\..*\.hanacloud\.ondemand\.com$", "HDLRE Coord Instance"),
    (r"^api\..*\.k8s.ondemand.com$", "K8S API Server"),
    (r"^kubernetes\.default\.svc$", "K8S API Server"),
    (r"^api(\.gateway)?\.orchestration\..*\.hanacloud.ondemand.com$", "Orc K8S API Server"),
]

ipnodes = {}
edges = {}

for cluster in ('orc', 'hdl', 'hana'):
    edges[cluster] = set()
    with open(f"{cluster}-outgoing.csv") as f:
        reader = csv.reader(f)
        next(reader)  # skip header
        for i, row in enumerate(reader):
            namespace = row[2]
            if namespace in ('monitoring','kube-system'):
                continue

            podname = row[3].split('-')
            if len(podname[-1]) == 5:
                podname = podname[0:-1]

            try:
                while int(podname[-1], 16) >= 0:
                    podname = podname[0:-1]
            except:
                pass

            name = "-".join(podname)
            if name == "":
                name = row[3]

            destname = row[5]
            if destname == "":
                destname = row[1]
            else:
                ipnodes[row[1]] = destname

            for codename in CODENAMES:
                if re.match(codename[0], name):
                    name = codename[1]
                if re.match(codename[0], destname):
                    destname = codename[1]

            ipnodes[row[4]] = name

            if (name, destname) not in edges:
                edges[cluster].add((name, destname))

for cluster in ('orc', 'hdl', 'hana'):
    nodes = {}
    dot = graphviz.Digraph(engine="sfdp")
    for (name, destname) in edges[cluster]:
        if name not in nodes:
            dot.node(name)
            nodes[name] = row

        if ipnodes.get(destname, "") != "":
            destname = ipnodes[destname]

        if destname not in nodes:
            dot.node(destname)
            nodes[destname] = row

        dot.edge(name, destname)

    dot.render(f"{cluster}-outgoing.gv")
