#!/usr/bin/env python3
"""Regenerate tiny, wholly original EPUB fixtures for ebook_text_language_test.go."""

from __future__ import annotations

from pathlib import Path
from zipfile import ZIP_DEFLATED, ZIP_STORED, ZipFile

OUT = Path(__file__).parent
PARAGRAPHS = {
    "eng": (
        "On a quiet morning the gardener opened the greenhouse and found every window covered with silver droplets. "
        "She carried a small notebook between the benches, counting new leaves and noting where the winter light reached the soil. "
        "Outside, the village baker stacked warm loaves beside the door while bicycles rattled across the old stone square. "
        "The garden had survived many storms, but today its narrow paths seemed especially calm and inviting. ",
        "After lunch the librarian visited the garden to borrow seeds for the courtyard behind the reading room. "
        "They spoke about the river, the changing weather, and the children who had built paper boats beside the bridge. "
        "A bright blue bird landed on the fence and waited until the conversation ended before flying toward the orchard. "
        "At sunset the gardener locked the gate, washed her tools, and wrote one final observation in her notebook. ",
    ),
    "fre": (
        "Ce matin, la jardinière a ouvert la serre et découvert de petites gouttes argentées sur chaque fenêtre. "
        "Elle portait son carnet entre les plantes, comptait les feuilles nouvelles et observait la lumière du soleil sur la terre. "
        "Devant la place du village, le boulanger préparait du pain tandis que des vélos traversaient les rues pavées. "
        "Le jardin avait connu plusieurs tempêtes, mais ses chemins semblaient particulièrement tranquilles et accueillants. ",
        "Après le déjeuner, la bibliothécaire est venue chercher des graines pour la cour derrière la salle de lecture. "
        "Elles ont parlé de la rivière, du temps changeant et des enfants qui fabriquaient des bateaux en papier près du pont. "
        "Un oiseau bleu s'est posé sur la clôture, puis il s'est envolé vers le verger lorsque la conversation s'est terminée. "
        "Au crépuscule, la jardinière a fermé la grille, nettoyé ses outils et écrit une dernière remarque dans son carnet. ",
    ),
    "spa": (
        "En una mañana tranquila, la jardinera abrió el invernadero y encontró gotas brillantes sobre todas las ventanas. "
        "Llevaba un cuaderno entre las plantas, contaba las hojas nuevas y observaba cómo entraba la luz del invierno. "
        "En la plaza del pueblo, el panadero colocaba pan caliente junto a la puerta mientras las bicicletas cruzaban la calle. "
        "El jardín había resistido muchas tormentas, pero sus caminos parecían especialmente serenos aquella mañana. ",
        "Después de comer, la bibliotecaria fue a buscar semillas para el patio que estaba detrás de la sala de lectura. "
        "Hablaron del río, del tiempo cambiante y de los niños que construían barcos de papel cerca del puente. "
        "Un pájaro azul se posó en la cerca y esperó hasta que terminó la conversación para volar hacia el huerto. "
        "Al atardecer, la jardinera cerró la puerta, lavó sus herramientas y escribió una última nota en el cuaderno. ",
    ),
    "rum": (
        "Într-o dimineață liniștită, grădinara a deschis sera și a găsit fiecare fereastră acoperită cu picături argintii. "
        "Ea purta un caiet printre straturile de flori, număra frunzele noi și observa cum lumina iernii ajungea până la pământ. "
        "Lângă piața satului, brutarul așeza pâine caldă pe raft, iar bicicletele treceau zgomotos peste pietrele vechi. "
        "Grădina trecuse prin multe furtuni, dar cărările ei păreau astăzi deosebit de calme și primitoare. ",
        "După prânz, bibliotecara a venit să împrumute semințe pentru curtea din spatele sălii de lectură. "
        "Au vorbit despre râu, despre vremea schimbătoare și despre copiii care făceau bărcuțe de hârtie lângă pod. "
        "O pasăre albastră s-a așezat pe gard și a așteptat sfârșitul conversației înainte să zboare către livadă. "
        "La apus, grădinara a încuiat poarta, și-a spălat uneltele și a scris o ultimă observație în caiet. ",
    ),
}


def chapter(language: str, second: bool = False) -> str:
    # Each chapter exceeds 1200 *visible* runes; text, not markup or metadata.
    paragraph = PARAGRAPHS[language][int(second)]
    return "".join(f"<p>{paragraph}</p>" for _ in range(5))


def xhtml(body: str, title: str = "Sample") -> bytes:
    return ("<?xml version='1.0' encoding='utf-8'?>"
            "<html xmlns='http://www.w3.org/1999/xhtml'>"
            f"<head><title>{title}</title><style>.page {{ color: blue; }}</style></head>"
            f"<body><section>{body}</section></body></html>").encode("utf-8")


def make(name: str, first: str, second: str | None, declared: str, short: bool = False) -> None:
    entries = [
        ("title.xhtml", "title", xhtml(chapter("eng"), "English title page")),
        ("cover.xhtml", "cover", xhtml(chapter("eng"), "English cover page")),
        ("front-matter.xhtml", "front", xhtml(chapter("eng"), "English preface")),
    ]
    if first:
        body1 = "<p>A brief page.</p>" if short else chapter(first)
        body2 = "<p>Another brief page.</p>" if short else chapter(second or first, second=True)
        entries += [("chapter1.xhtml", "chapter1", xhtml(body1)),
                    ("chapter2.xhtml", "chapter2", xhtml(body2))]
    manifest = "".join(f"<item id='{item_id}' href='{filename}' media-type='application/xhtml+xml'/>"
                       for filename, item_id, _ in entries)
    spine = "".join(f"<itemref idref='{item_id}'/>" for _, item_id, _ in entries)
    opf = ("<?xml version='1.0' encoding='utf-8'?>"
           "<package xmlns='http://www.idpf.org/2007/opf' version='3.0' unique-identifier='id'>"
           "<metadata xmlns:dc='http://purl.org/dc/elements/1.1/'>"
           f"<dc:title>English catalogue label</dc:title><dc:language>{declared}</dc:language>"
           "</metadata><manifest>" + manifest + "</manifest><spine>" + spine + "</spine></package>")
    with ZipFile(OUT / name, "w") as archive:
        archive.writestr("mimetype", "application/epub+zip", compress_type=ZIP_STORED)
        archive.writestr("META-INF/container.xml",
                         "<container><rootfiles><rootfile full-path='OEBPS/content.opf'/></rootfiles></container>",
                         compress_type=ZIP_DEFLATED)
        archive.writestr("OEBPS/content.opf", opf, compress_type=ZIP_DEFLATED)
        for filename, _, data in entries:
            archive.writestr("OEBPS/" + filename, data, compress_type=ZIP_DEFLATED)


if __name__ == "__main__":
    PARAGRAPHS["mixed"] = (PARAGRAPHS["eng"][0] + PARAGRAPHS["rum"][0],
                           PARAGRAPHS["eng"][1] + PARAGRAPHS["rum"][1])
    make("english.epub", "eng", "eng", "en")
    make("french.epub", "fre", "fre", "fr")
    make("romanian.epub", "rum", "rum", "ro")
    make("spanish.epub", "spa", "spa", "es")
    make("romanian-en-opf.epub", "rum", "rum", "en")
    make("mixed.epub", "eng", "rum", "en")
    make("intra-mixed.epub", "mixed", "mixed", "en")
    make("short.epub", "eng", "eng", "en", short=True)
    make("front-only.epub", "", None, "en")
