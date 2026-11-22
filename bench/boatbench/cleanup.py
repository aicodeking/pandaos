"""Bounded cleanup of explicitly owned fixture or benchmark VM identifiers."""


def cleanup_vms(driver, pending, errors, log, attempts=2):
    """Retry failures once; retain unresolved IDs/errors and never re-delete a success."""
    for _ in range(attempts):
        for vm in list(pending):
            try:
                driver.delete(vm)
            except Exception as exc:
                errors[vm] = f"{type(exc).__name__}: {exc}"
                log(f"delete {vm}: {exc}")
            else:
                pending.remove(vm)
                errors.pop(vm, None)
        if not pending:
            break
