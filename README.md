# SD-WAN Operator: Helm charts

## Setup

Register the repository with Helm.

``` console
helm repo add dcontroller https://l7mp.github.io/sdwan-operator/
helm repo update
```

## Install

Deploy the SD-WAN operator.

``` console
helm install sdwan-operator sdwan-operator/sdwan-operator
```

## License

Copyright 2025 by its authors. Some rights reserved. See [AUTHORS](AUTHORS).

Apache License - see [LICENSE](LICENSE) for full text.
