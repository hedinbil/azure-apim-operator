cd ../charts
helm package aks-openapi-operator
helm push aks-openapi-operator-0.4.0.tgz oci://hedinit.azurecr.io/helm-charts