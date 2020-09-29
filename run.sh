secret_name=$(kubectl get events -o=jsonpath='{.items[0].involvedObject.name}')
if [ "$secret_name" != "gitlab-registry2" ]; then
    echo "not equal"
fi