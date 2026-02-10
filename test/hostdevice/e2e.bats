#!/usr/bin/env bats

load 'test_helper/bats-support/load'
load 'test_helper/bats-assert/load'

# ---- GLOBAL CLEANUP ----

teardown() {
  if [[ -z "$BATS_TEST_COMPLETED" || "$BATS_TEST_COMPLETED" -ne 1 ]] && [[ -z "$BATS_TEST_SKIPPED" ]]; then
    dump_debug_info_on_failure
  fi
  cleanup_k8s_resources
  cleanup_dummy_interfaces
  cleanup_bpf_programs
  # The driver is rate limited to updates with interval of atleast 5 seconds. So
  # we need to sleep for an equivalent amount of time to ensure state from a
  # previous test is cleared up and old (non-existent) devices have been removed
  # from the ResourceSlice. This seems to only be an an issue of the test where
  # we create "dummy" interfaces which disappear if the network namespace is
  # deleted.
  sleep 5
}

dump_debug_info_on_failure() {
  echo "--- Test failed. Dumping debug information ---"

  echo "--- DeviceClasses ---"
  for dc in $(kubectl get deviceclass -o name); do
    echo "--- $dc ---"
    kubectl get "$dc" -o yaml
  done

  echo "--- ResourceSlices ---"
  for rs in $(kubectl get resourceslice -o name); do
    echo "--- $rs ---"
    kubectl get "$rs" -o yaml
  done

  echo "--- ResourceClaims ---"
  for rc in $(kubectl get resourceclaim -o name); do
    echo "--- $rc ---"
    kubectl get "$rc" -o yaml
  done

  echo "--- Pods Description ---"
  for pod in $(kubectl get pods -o name); do
    echo "--- $pod ---"
    kubectl describe "$pod"
  done

  echo "--- End of debug information ---"
}

cleanup_k8s_resources() {
  kubectl delete -f "$BATS_TEST_DIRNAME"/../tests/manifests --ignore-not-found --recursive || true
}

cleanup_dummy_interfaces() {
  for node in "$CLUSTER_NAME"-worker "$CLUSTER_NAME"-worker2; do
    docker exec "$node" bash -c '
      for dev in $(ip -br link show type dummy | awk "{print \$1}"); do
        ip link delete "$dev" || echo "Failed to delete $dev"
      done
    '
  done
}

# ---- TESTS ----

@test "dummy interface with IP addresses ResourceClaim" {
  docker exec "$CLUSTER_NAME"-worker bash -c "ip link add dummy0 type dummy"
  docker exec "$CLUSTER_NAME"-worker bash -c "ip link set up dev dummy0"

  kubectl apply -f "$BATS_TEST_DIRNAME"/../tests/manifests/deviceclass.yaml
  kubectl apply -f "$BATS_TEST_DIRNAME"/../tests/manifests/resourceclaim.yaml
  kubectl wait --timeout=30s --for=condition=ready pods -l app=pod

  run kubectl exec pod1 -- ip addr show eth99
  assert_success
  assert_output --partial "169.254.169.13"

  run kubectl get resourceclaims dummy-interface-static-ip -o=jsonpath='{.status.devices[0].networkData.ips[*]}'
  assert_success
  assert_output --partial "169.254.169.13"
}