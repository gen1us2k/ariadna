package osm

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/julienschmidt/httprouter"
	geo "github.com/kellydunn/golang-geo"
	"github.com/maddevsio/ariadna/config"
	"github.com/maddevsio/ariadna/elastic"
	"github.com/maddevsio/ariadna/osm/handler"
	"github.com/maddevsio/ariadna/osm/parser"
	"github.com/missinglink/gosmparse"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// Importer struct represents needed values to import data to elasticsearch
type (
	Importer struct {
		handler   *handler.Handler
		parser    *parser.Parser
		config    *config.Ariadna
		e         *elastic.Client
		eg        errgroup.Group
		logger    *logrus.Logger
		countries []country
	}
	country struct {
		name  string
		towns []city
		geom  *geo.Polygon
	}
	city struct {
		name      string
		placeType string
		geom      *geo.Polygon
		districts []district
	}
	district struct {
		name string
		geom *geo.Polygon
	}
)

// NewImporter creates new instance of importer
func NewImporter(c *config.Ariadna) (*Importer, error) {
	i := &Importer{config: c, logger: logrus.New()}
	if err := i.download(); err != nil {
		return nil, err
	}
	p, err := parser.NewParser(c.OSMFilename)
	if err != nil {
		return nil, err
	}
	i.parser = p
	e, err := elastic.New(c)
	if err != nil {
		return nil, err
	}
	i.e = e
	i.handler = handler.New()
	i.logger.Info("parser initialized")
	return i, nil
}
func (i *Importer) parse() error {
	return i.parser.Parse(i.handler)
}
func (i *Importer) updateIndices() error {
	return i.e.UpdateIndex()
}

// Start starts parsing
func (i *Importer) Start() error {
	if err := i.parse(); err != nil {
		return err
	}
	if err := i.updateIndices(); err != nil {
		return err
	}
	i.areasToPolygons()
	i.eg.Go(i.crossRoadsToElastic)
	i.eg.Go(i.nodesToElastic)
	i.eg.Go(i.waysToElastic)
	return nil
}

// WaitStop is wrapper around waitgroup
func (i *Importer) WaitStop() {
	i.eg.Wait()
}
func (i *Importer) Done() error {
	return i.e.DeleteIndices()
}
func uniqString(list []string) []string {
	uniqueSet := make(map[string]bool)
	for _, x := range list {
		uniqueSet[x] = true
	}
	result := make([]string, 0, len(uniqueSet))
	for x := range uniqueSet {
		result = append(result, x)
	}
	return result
}
func (i *Importer) areasToPolygons() {
	i.logger.Info("started to build country index")
	for _, cn := range i.handler.Countries {
		if cn.Tags["name"] != i.config.ImportCountry {
			continue
		}
		countryPolygon := i.relationToPolygon(cn)

		f, err := os.Create(cn.Tags["name"])
		if err != nil {
			log.Fatal(err)
		}
		for _, point := range countryPolygon.Points() {
			f.Write([]byte(fmt.Sprintf("%v,%v\n", point.Lng(), point.Lat())))
		}
		f.Close()
		c := country{
			name: cn.Tags["name"],
			geom: countryPolygon,
		}
		for _, area := range i.handler.Areas {
			areaPolygon := i.relationToPolygon(area)
			city := city{
				name:      area.Tags["name"],
				geom:      areaPolygon,
				placeType: area.Tags["place"],
			}
			for _, dist := range i.handler.Districts {
				districtPolygon := i.wayToPolygon(dist)
				if areaPolygon.Contains(districtPolygon.Points()[1]) {
					d := district{name: dist.Tags["name"], geom: districtPolygon}
					city.districts = append(city.districts, d)
				}
			}
			if countryPolygon.Contains(areaPolygon.Points()[1]) {
				c.towns = append(c.towns, city)
			}

		}
		i.countries = append(i.countries, c)

	}
	i.logger.Info("finished to build country index")
}
func (i *Importer) relationToPolygon(area gosmparse.Relation) *geo.Polygon {
	var points []*geo.Point
	for _, member := range area.Members {
		node, ok := i.handler.Nodes[member.ID]
		if ok {
			points = append(points, geo.NewPoint(node.Lat, node.Lon))
		}
		if !ok {
			way := i.handler.FullWays[member.ID]
			for _, nodeID := range way.NodeIDs {
				node := i.handler.Nodes[nodeID]
				points = append(points, geo.NewPoint(node.Lat, node.Lon))
			}
		}

	}
	return geo.NewPolygon(points)
}
func (i *Importer) wayToPolygon(way gosmparse.Way) *geo.Polygon {
	var points []*geo.Point
	for _, nodeID := range way.NodeIDs {
		node := i.handler.Nodes[nodeID]
		points = append(points, geo.NewPoint(node.Lat, node.Lon))
	}
	return geo.NewPolygon(points)
}

func (i *Importer) StartWebServer() error {
	router := httprouter.New()
	router.GET("/api/search/:query", i.geoCodeHandler)
	router.GET("/api/reverse/:lat/:lon", i.reverseGeoCodeHandler)
	router.NotFound = http.FileServer(http.Dir("public"))
	http.ListenAndServe(":8080", router)
	return nil
}
