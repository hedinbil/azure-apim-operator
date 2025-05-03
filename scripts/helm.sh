cd ../charts
helm package aks-apim-operator
helm push aks-apim-operator-0.17.0.tgz oci://hedinit.azurecr.io/helm-charts