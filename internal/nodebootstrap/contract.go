package nodebootstrap

// HelperContractVersion identifies the interface used by generated systemd units,
// including the runtime database address and locality environment variables.
// Bump it when a service/helper interface changes incompatibly. It is separate
// from source commits: compatible helper fixes need not match the renderer SHA.
const HelperContractVersion = "1"
