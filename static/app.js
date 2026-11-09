// Votes save in place so the list doesn't reload and reorder while reading.
// Without this script the forms still work as plain posts.
document.addEventListener("submit", async (event) => {
  const form = event.target;
  if (!form.classList.contains("vote")) return;
  event.preventDefault();
  const response = await fetch(form.action, {
    method: "POST",
    body: new FormData(form),
    headers: { "X-Vote": "1" },
  }).catch(() => null);
  if (!response || !response.ok) return form.submit();

  const votes = form.closest(".votes");
  const value = form.elements.rating.value;
  const rating = votes.dataset.rating === value ? "0" : value;
  votes.dataset.rating = rating;
  for (const f of votes.querySelectorAll("form")) {
    f.querySelector("button").setAttribute("aria-pressed", String(f.elements.rating.value === rating));
  }
});
