// Selection helpers for the job table.
//
// The table fragment is swapped wholesale by the poller, which would wipe any
// checkboxes the operator has ticked. noSelection() is used as an htmx trigger
// filter so polling pauses for as long as a selection exists; the swap then
// only happens when there is nothing to lose.

function selectedBoxes() {
    return document.querySelectorAll("#jobs input[name='ids']:checked");
}

// Referenced from hx-trigger="every 3s [noSelection()]".
function noSelection() {
    return selectedBoxes().length === 0;
}

function toggleAll(source) {
    document
        .querySelectorAll("#jobs input[name='ids']")
        .forEach((box) => {
            box.checked = source.checked;
        });
    refreshSelection();
}

function refreshSelection() {
    const hint = document.getElementById("selection-hint");
    if (!hint) {
        return;
    }

    const count = selectedBoxes().length;
    hint.textContent = count === 0
        ? ""
        : `${count} selected — auto-refresh paused`;
}

// A swap replaces the hint element, so recompute once the new markup is live.
document.addEventListener("htmx:afterSwap", refreshSelection);
document.addEventListener("DOMContentLoaded", refreshSelection);
