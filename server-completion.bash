# she on the server (she-server.py), read from ~/.bashrc of a login. The keys
# are those of the PC's Git Bash (completion.bash): she [NAME] < then Tab
# makes she [NAME] <<<'|', Tab again switches to <<'E' ... E, Enter in that
# body adds a line, and Tab inside the quotes after <<< completes from the
# bash kept for NAME (she --complete). Tab on the first argument completes
# the names of the kept bashes (she -l). The PC's she wrapper (a password
# read by bash) is left out: she here reads it itself.
. ~/lab-tools/she/completion.bash
unset -f she
_she_server() {
  local x
  local -a n=(-l)
  if [[ ${COMP_LINE:0:COMP_POINT} == *'<<'* ]]; then
    _she "$@"
    return
  fi
  ((COMP_CWORD == 1)) || return 0
  while IFS=$'\t' read -r x _; do n+=("$x"); done < <(command she -l 2>/dev/null)
  mapfile -t COMPREPLY < <(compgen -W "${n[*]}" -- "$2")
}
complete -F _she_server she
